// Package configstore owns the validate-before-write lifecycle used by the
// management API. It deliberately keeps secret values out of the browser
// document while preserving them when a redacted document is applied.
package configstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"asterferry/internal/config"
	"asterferry/internal/diagnostics"
)

const (
	SchemaVersion      = 1
	MaxDocumentBytes   = 1 << 20
	RedactedValue      = "<redacted>"
	backupSuffix       = ".bak"
	maxDiffOutputLines = 400
)

var (
	ErrRevisionConflict = errors.New("configuration revision changed")
	ErrReadOnly         = errors.New("configuration file is read-only")
	ErrNoBackup         = errors.New("no configuration backup is available")
	ErrInvalid          = errors.New("configuration is invalid")
	ErrSecretField      = errors.New("secret fields cannot be changed from the web")
)

type Snapshot struct {
	SchemaVersion   int    `json:"schema_version"`
	Role            string `json:"role"`
	Revision        string `json:"revision"`
	Writable        bool   `json:"writable"`
	BackupAvailable bool   `json:"backup_available"`
	YAML            string `json:"yaml"`
	Values          any    `json:"values"`
}

type Validation struct {
	SchemaVersion int      `json:"schema_version"`
	Role          string   `json:"role"`
	Revision      string   `json:"revision"`
	Changed       bool     `json:"changed"`
	Diff          string   `json:"diff,omitempty"`
	Warnings      []string `json:"warnings,omitempty"`
}

type ApplyResult struct {
	SchemaVersion int    `json:"schema_version"`
	Role          string `json:"role"`
	Revision      string `json:"revision"`
	Backup        bool   `json:"backup"`
}

type Manager struct {
	mu   sync.Mutex
	path string
	role string
}

func New(path string) (*Manager, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || path == "" {
		return nil, errors.New("configuration path is required")
	}
	current, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read configuration: %w", err)
	}
	c, err := config.LoadBytes(current, path)
	if err != nil {
		return nil, err
	}
	return &Manager{path: path, role: c.Role}, nil
}

func (m *Manager) Snapshot() (Snapshot, error) {
	if m == nil {
		return Snapshot{}, errors.New("configuration manager is unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, err := os.ReadFile(m.path)
	if err != nil {
		return Snapshot{}, fmt.Errorf("read configuration: %w", err)
	}
	redacted, err := redactYAML(current)
	if err != nil {
		return Snapshot{}, fmt.Errorf("redact configuration: %w", err)
	}
	values, err := yamlValues(redacted)
	if err != nil {
		return Snapshot{}, fmt.Errorf("decode configuration values: %w", err)
	}
	return Snapshot{
		SchemaVersion:   SchemaVersion,
		Role:            m.role,
		Revision:        revision(current),
		Writable:        probeWritable(m.path),
		BackupAvailable: fileExists(m.backupPath()),
		YAML:            string(redacted),
		Values:          values,
	}, nil
}

// JSONToYAML converts the structured editor payload into the same strict YAML
// document accepted by the file loader. The configuration validator remains
// authoritative; this helper only handles representation conversion.
func JSONToYAML(raw []byte) ([]byte, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("multiple JSON values are not allowed")
		}
		return nil, err
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, errors.New("structured configuration must be a JSON object")
	}
	return yaml.Marshal(value)
}

func (m *Manager) Validate(expectedRevision string, candidate []byte) (Validation, error) {
	if m == nil {
		return Validation{}, errors.New("configuration manager is unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, err := os.ReadFile(m.path)
	if err != nil {
		return Validation{}, fmt.Errorf("read configuration: %w", err)
	}
	return m.validateLocked(current, expectedRevision, candidate)
}

func (m *Manager) Apply(expectedRevision string, candidate []byte) (ApplyResult, error) {
	if m == nil {
		return ApplyResult{}, errors.New("configuration manager is unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, err := os.ReadFile(m.path)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("read configuration: %w", err)
	}
	validation, err := m.validateLocked(current, expectedRevision, candidate)
	if err != nil {
		return ApplyResult{}, err
	}
	if !probeWritable(m.path) {
		return ApplyResult{}, ErrReadOnly
	}
	restored, err := restoreRedactedSecrets(candidate, current)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	info, err := os.Stat(m.path)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("stat configuration: %w", err)
	}
	mode := info.Mode().Perm()
	if mode == 0 {
		mode = 0o600
	}
	if err := writeAtomic(m.backupPath(), current, mode); err != nil {
		return ApplyResult{}, fmt.Errorf("write configuration backup: %w", err)
	}
	if err := writeAtomic(m.path, restored, mode); err != nil {
		return ApplyResult{}, fmt.Errorf("write configuration: %w", err)
	}
	return ApplyResult{
		SchemaVersion: SchemaVersion,
		Role:          validation.Role,
		Revision:      revision(restored),
		Backup:        true,
	}, nil
}

func (m *Manager) Rollback(expectedRevision string) (ApplyResult, error) {
	if m == nil {
		return ApplyResult{}, errors.New("configuration manager is unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, err := os.ReadFile(m.path)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("read configuration: %w", err)
	}
	if expectedRevision != "" && revision(current) != expectedRevision {
		return ApplyResult{}, ErrRevisionConflict
	}
	if !probeWritable(m.path) {
		return ApplyResult{}, ErrReadOnly
	}
	backup, err := os.ReadFile(m.backupPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ApplyResult{}, ErrNoBackup
		}
		return ApplyResult{}, fmt.Errorf("read configuration backup: %w", err)
	}
	if _, err := m.validateLocked(current, "", backup); err != nil {
		return ApplyResult{}, fmt.Errorf("%w: backup cannot be restored: %v", ErrInvalid, err)
	}
	info, err := os.Stat(m.path)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("stat configuration: %w", err)
	}
	mode := info.Mode().Perm()
	if mode == 0 {
		mode = 0o600
	}
	if err := writeAtomic(m.backupPath(), current, mode); err != nil {
		return ApplyResult{}, fmt.Errorf("write rollback backup: %w", err)
	}
	if err := writeAtomic(m.path, backup, mode); err != nil {
		return ApplyResult{}, fmt.Errorf("restore configuration: %w", err)
	}
	return ApplyResult{SchemaVersion: SchemaVersion, Role: m.role, Revision: revision(backup), Backup: true}, nil
}

func (m *Manager) validateLocked(current []byte, expectedRevision string, candidate []byte) (Validation, error) {
	if len(candidate) == 0 {
		return Validation{}, fmt.Errorf("%w: document is empty", ErrInvalid)
	}
	if len(candidate) > MaxDocumentBytes {
		return Validation{}, fmt.Errorf("%w: document exceeds %d bytes", ErrInvalid, MaxDocumentBytes)
	}
	currentRevision := revision(current)
	if expectedRevision != "" && expectedRevision != currentRevision {
		return Validation{}, ErrRevisionConflict
	}
	restored, err := restoreRedactedSecrets(candidate, current)
	if err != nil {
		return Validation{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	c, err := config.LoadBytes(restored, m.path)
	if err != nil {
		return Validation{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := config.ApplyEnv(c); err != nil {
		return Validation{}, fmt.Errorf("%w: environment overrides: %v", ErrInvalid, err)
	}
	if c.Role != m.role {
		return Validation{}, fmt.Errorf("%w: role cannot change from %s to %s", ErrInvalid, m.role, c.Role)
	}
	if c.Role == config.RoleGateway {
		if _, err := c.ResolveGateway(); err != nil {
			return Validation{}, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
	} else if _, err := c.ResolveAgent(); err != nil {
		return Validation{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	report := diagnostics.Check(c, true)
	if report.Errors() > 0 {
		messages := make([]string, 0, report.Errors())
		for _, finding := range report.Findings {
			if finding.Severity == diagnostics.SeverityError {
				messages = append(messages, finding.Path+": "+finding.Message)
			}
		}
		return Validation{}, fmt.Errorf("%w: %s", ErrInvalid, strings.Join(messages, "; "))
	}
	redactedCurrent, err := redactYAML(current)
	if err != nil {
		return Validation{}, fmt.Errorf("%w: redact current configuration: %v", ErrInvalid, err)
	}
	redactedCandidate, err := redactYAML(restored)
	if err != nil {
		return Validation{}, fmt.Errorf("%w: redact candidate configuration: %v", ErrInvalid, err)
	}
	warnings := make([]string, 0)
	for _, finding := range report.Findings {
		if finding.Severity == diagnostics.SeverityWarn {
			warnings = append(warnings, finding.Path+": "+finding.Message)
		}
	}
	return Validation{
		SchemaVersion: SchemaVersion,
		Role:          c.Role,
		Revision:      currentRevision,
		Changed:       !bytes.Equal(current, restored),
		Diff:          lineDiff(string(redactedCurrent), string(redactedCandidate)),
		Warnings:      warnings,
	}, nil
}

func (m *Manager) backupPath() string { return m.path + backupSuffix }

func revision(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum[:])
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func probeWritable(path string) bool {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".asterferry-write-probe-*")
	if err != nil {
		return false
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	_ = os.Remove(tmpPath)
	return true
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".asterferry-config-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err == nil {
		return nil
	} else {
		// Windows cannot replace an existing file with Rename. The old file is
		// already protected by the caller's backup; restore is still attempted
		// if the second rename fails.
		if removeErr := os.Remove(path); removeErr != nil {
			return err
		}
		if renameErr := os.Rename(tmpPath, path); renameErr != nil {
			return renameErr
		}
	}
	return nil
}

func redactYAML(raw []byte) ([]byte, error) {
	doc, err := decodeDocument(raw)
	if err != nil {
		return nil, err
	}
	redactNode(doc)
	stripComments(doc)
	return encodeDocument(doc)
}

func restoreRedactedSecrets(candidate, current []byte) ([]byte, error) {
	doc, err := decodeDocument(candidate)
	if err != nil {
		return nil, err
	}
	base, err := decodeDocument(current)
	if err != nil {
		return nil, err
	}
	if err := restoreNode(doc, base); err != nil {
		return nil, err
	}
	return encodeDocument(doc)
}

func decodeDocument(raw []byte) (*yaml.Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("multiple YAML documents are not allowed")
		}
		return nil, err
	}
	return &doc, nil
}

func encodeDocument(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func yamlValues(raw []byte) (any, error) {
	doc, err := decodeDocument(raw)
	if err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return nil, errors.New("configuration document is empty")
	}
	return yamlValue(doc.Content[0])
}

func yamlValue(node *yaml.Node) (any, error) {
	switch node.Kind {
	case yaml.MappingNode:
		result := make(map[string]any, len(node.Content)/2)
		for i := 0; i+1 < len(node.Content); i += 2 {
			value, err := yamlValue(node.Content[i+1])
			if err != nil {
				return nil, err
			}
			result[node.Content[i].Value] = value
		}
		return result, nil
	case yaml.SequenceNode:
		result := make([]any, 0, len(node.Content))
		for _, child := range node.Content {
			value, err := yamlValue(child)
			if err != nil {
				return nil, err
			}
			result = append(result, value)
		}
		return result, nil
	case yaml.ScalarNode:
		var value any
		if err := node.Decode(&value); err != nil {
			return nil, err
		}
		return value, nil
	case yaml.AliasNode:
		if node.Alias == nil {
			return nil, errors.New("YAML alias has no target")
		}
		return yamlValue(node.Alias)
	default:
		return nil, fmt.Errorf("unsupported YAML node kind %d", node.Kind)
	}
}

func redactNode(node *yaml.Node) {
	redactNodeSeen(node, make(map[*yaml.Node]bool))
}

func redactNodeSeen(node *yaml.Node, seen map[*yaml.Node]bool) {
	if node == nil {
		return
	}
	if seen[node] {
		return
	}
	seen[node] = true
	if node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if key.Value == "password" {
				node.Content[i+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: RedactedValue}
				continue
			}
			redactNodeSeen(value, seen)
		}
	}
	for _, child := range node.Content {
		redactNodeSeen(child, seen)
	}
	if node.Kind == yaml.AliasNode {
		redactNodeSeen(node.Alias, seen)
	}
}

func stripComments(node *yaml.Node) {
	stripCommentsSeen(node, make(map[*yaml.Node]bool))
}

func stripCommentsSeen(node *yaml.Node, seen map[*yaml.Node]bool) {
	if node == nil || seen[node] {
		return
	}
	seen[node] = true
	node.HeadComment = ""
	node.LineComment = ""
	node.FootComment = ""
	for _, child := range node.Content {
		stripCommentsSeen(child, seen)
	}
	if node.Kind == yaml.AliasNode {
		stripCommentsSeen(node.Alias, seen)
	}
}

func restoreNode(node, base *yaml.Node) error {
	if node == nil || base == nil {
		return nil
	}
	if node.Kind == yaml.DocumentNode && base.Kind == yaml.DocumentNode {
		if len(node.Content) == 0 || len(base.Content) == 0 {
			return nil
		}
		return restoreNode(node.Content[0], base.Content[0])
	}
	if node.Kind == yaml.MappingNode && base.Kind == yaml.MappingNode {
		baseValues := mappingValues(base)
		seen := make(map[string]bool)
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			seen[key.Value] = true
			baseValue := baseValues[key.Value]
			if key.Value == "password" {
				if baseValue == nil {
					return errors.New("password field has no existing value")
				}
				if value.Kind != yaml.ScalarNode || (value.Value != RedactedValue && value.Value != baseValue.Value) {
					return ErrSecretField
				}
				node.Content[i+1] = cloneNode(baseValue)
				continue
			}
			if baseValue != nil {
				if err := restoreNode(value, baseValue); err != nil {
					return err
				}
			}
		}
		for key, value := range baseValues {
			if key == "password" && !seen[key] {
				node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, cloneNode(value))
			}
		}
	}
	if node.Kind == yaml.SequenceNode && base.Kind == yaml.SequenceNode {
		for i, child := range node.Content {
			if i < len(base.Content) {
				if err := restoreNode(child, base.Content[i]); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func mappingValues(node *yaml.Node) map[string]*yaml.Node {
	values := make(map[string]*yaml.Node)
	if node == nil || node.Kind != yaml.MappingNode {
		return values
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		values[node.Content[i].Value] = node.Content[i+1]
	}
	return values
}

func cloneNode(node *yaml.Node) *yaml.Node {
	if node == nil {
		return nil
	}
	clone := *node
	clone.Content = make([]*yaml.Node, len(node.Content))
	for i, child := range node.Content {
		clone.Content[i] = cloneNode(child)
	}
	return &clone
}

func lineDiff(current, candidate string) string {
	if current == candidate {
		return ""
	}
	oldLines := strings.Split(strings.TrimSuffix(current, "\n"), "\n")
	newLines := strings.Split(strings.TrimSuffix(candidate, "\n"), "\n")
	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(oldLines)-prefix && suffix < len(newLines)-prefix && oldLines[len(oldLines)-1-suffix] == newLines[len(newLines)-1-suffix] {
		suffix++
	}
	var out strings.Builder
	out.WriteString("--- current\n+++ candidate\n")
	lines := 0
	for _, line := range oldLines[prefix : len(oldLines)-suffix] {
		if lines >= maxDiffOutputLines {
			break
		}
		out.WriteString("-" + line + "\n")
		lines++
	}
	for _, line := range newLines[prefix : len(newLines)-suffix] {
		if lines >= maxDiffOutputLines {
			break
		}
		out.WriteString("+" + line + "\n")
		lines++
	}
	if lines >= maxDiffOutputLines {
		out.WriteString("... diff truncated ...\n")
	}
	return out.String()
}
