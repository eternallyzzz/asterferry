package bootstrap

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"asterferry/internal/bundle"
	"asterferry/internal/config"
	"gopkg.in/yaml.v3"
)

type MigrateOptions struct {
	Dir    string
	DryRun bool
}

type MigrateResult struct {
	Changed []string
}

func Migrate(opts MigrateOptions) (MigrateResult, error) {
	b, err := bundle.Open(opts.Dir)
	if err != nil {
		return MigrateResult{}, err
	}
	result := MigrateResult{}
	for _, item := range []struct {
		role string
		path string
	}{{config.RoleGateway, b.GatewayConfig}, {config.RoleAgent, b.AgentConfig}} {
		raw, err := os.ReadFile(item.path)
		if err != nil {
			return MigrateResult{}, fmt.Errorf("read %s configuration: %w", item.role, err)
		}
		viewerPath := filepath.ToSlash(filepath.Join("..", "secrets", item.role, "management-viewer.token"))
		updated, changed, err := migrateConfigDocument(raw, viewerPath)
		if err != nil {
			return MigrateResult{}, fmt.Errorf("migrate %s configuration: %w", item.role, err)
		}
		if !changed {
			continue
		}
		result.Changed = append(result.Changed, item.path, filepath.Join(b.Root, filepath.FromSlash(filepath.Join("secrets", item.role, "management-viewer.token"))))
		if opts.DryRun {
			continue
		}
		if err := writeSecretIfMissing(filepath.Join(b.Root, "secrets", item.role, "management-viewer.token")); err != nil {
			return MigrateResult{}, fmt.Errorf("write %s viewer token: %w", item.role, err)
		}
		if err := atomicBackupWrite(item.path, updated); err != nil {
			return MigrateResult{}, fmt.Errorf("write %s configuration: %w", item.role, err)
		}
	}
	return result, nil
}

func migrateConfigDocument(raw []byte, viewerPath string) ([]byte, bool, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		return nil, false, err
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, false, errors.New("multiple YAML documents are not allowed")
		}
		return nil, false, err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, false, errors.New("configuration document must be a YAML mapping")
	}
	root := doc.Content[0]
	management := mappingValue(root, "management")
	if management == nil || management.Kind != yaml.MappingNode {
		return nil, false, errors.New("management section is required")
	}
	auth := mappingValue(management, "auth")
	viewer := ""
	if auth != nil && auth.Kind == yaml.MappingNode {
		viewer = scalarValue(mappingValue(auth, "viewer_token_file"))
	}
	if viewer == "" {
		viewer = scalarValue(mappingValue(management, "viewer_token_file"))
	}
	if viewer != "" {
		return raw, false, nil
	}
	admin := ""
	if auth != nil && auth.Kind == yaml.MappingNode {
		admin = scalarValue(mappingValue(auth, "admin_token_file"))
	}
	legacy := scalarValue(mappingValue(management, "auth_token_file"))
	if admin == "" {
		admin = legacy
	}
	if admin == "" {
		return nil, false, errors.New("management admin token path is missing")
	}
	if auth == nil {
		auth = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		setMappingValue(management, "auth", auth)
	}
	setMappingValue(auth, "admin_token_file", scalarNode(admin))
	setMappingValue(auth, "viewer_token_file", scalarNode(viewerPath))
	removeMappingValue(management, "auth_token_file")
	removeMappingValue(management, "viewer_token_file")
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, false, err
	}
	if err := enc.Close(); err != nil {
		return nil, false, err
	}
	return out.Bytes(), true, nil
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func setMappingValue(node *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			node.Content[i+1] = value
			return
		}
	}
	node.Content = append(node.Content, scalarNode(key), value)
}

func removeMappingValue(node *yaml.Node, key string) {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			node.Content = append(node.Content[:i], node.Content[i+2:]...)
			return
		}
	}
}

func scalarNode(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}

func scalarValue(node *yaml.Node) string {
	if node == nil || node.Kind != yaml.ScalarNode {
		return ""
	}
	return strings.TrimSpace(node.Value)
}

func writeSecretIfMissing(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return err
	}
	encoded := fmt.Sprintf("%x", data)
	return writePrivateFile(path, []byte(encoded+"\n"))
}

func writePrivateFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func atomicBackupWrite(path string, data []byte) error {
	current, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if err := atomicWrite(path+".bak", current, info.Mode().Perm()); err != nil {
		return err
	}
	return atomicWrite(path, data, info.Mode().Perm())
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".asterferry-migrate-*")
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
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
