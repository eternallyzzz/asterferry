package domain

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
)

func (o ObservedState) Validate() error {
	if o.SchemaVersion != CurrentControlProtocolVersion {
		return &ApplyError{Code: "unknown_schema", Path: "schema_version", Message: "observed schema version is unsupported"}
	}
	if err := ValidateID(o.NodeID, "node_id"); err != nil {
		return err
	}
	if o.Healthy && o.Degraded {
		return &ApplyError{Code: "invalid_observed_state", Path: "healthy", Message: "observed state cannot be healthy and degraded at the same time"}
	}
	if o.LastError != nil {
		if strings.TrimSpace(o.LastError.Code) == "" || len(o.LastError.Code) > 128 || len(o.LastError.Message) > 2048 {
			return &ApplyError{Code: "invalid_observed_error", Path: "last_error", Message: "observed error fields are invalid"}
		}
	}
	if len(o.Sessions) > 4096 || len(o.Listeners) > 4096 {
		return &ApplyError{Code: "observed_state_too_large", Message: "observed state contains too many entries"}
	}
	seenSessions := make(map[string]struct{}, len(o.Sessions))
	for i, session := range o.Sessions {
		if err := ValidateID(session.ID, fmt.Sprintf("sessions[%d].id", i)); err != nil {
			return err
		}
		if _, exists := seenSessions[session.ID]; exists {
			return &ApplyError{Code: "duplicate_session", Path: fmt.Sprintf("sessions[%d].id", i), Message: "session id is duplicated"}
		}
		seenSessions[session.ID] = struct{}{}
		if session.PeerID != "" {
			if err := ValidateID(session.PeerID, fmt.Sprintf("sessions[%d].peer_id", i)); err != nil {
				return err
			}
		}
		if session.Streams < 0 || session.Streams > 1<<20 {
			return &ApplyError{Code: "invalid_session", Path: fmt.Sprintf("sessions[%d].streams", i), Message: "session stream count is invalid"}
		}
	}
	seenListeners := make(map[string]struct{}, len(o.Listeners))
	for i, listener := range o.Listeners {
		if err := validateListenerState(listener); err != nil {
			return &ApplyError{Code: "invalid_listener_state", Path: fmt.Sprintf("listeners[%d]", i), Message: err.Error()}
		}
		key := fmt.Sprintf("%s|%s|%d", listener.Protocol, normalizedIP(listener.Bind), listener.Port)
		if _, exists := seenListeners[key]; exists {
			return &ApplyError{Code: "duplicate_listener_state", Path: fmt.Sprintf("listeners[%d]", i), Message: "listener state is duplicated"}
		}
		seenListeners[key] = struct{}{}
	}
	return nil
}

func validateListenerState(listener ListenerState) error {
	if listener.Protocol != ProtocolTCP && listener.Protocol != ProtocolUDP {
		return errors.New("listener protocol must be tcp or udp")
	}
	if listener.Port == 0 {
		return errors.New("listener port must be non-zero")
	}
	if listener.Bind != strings.TrimSpace(listener.Bind) {
		return errors.New("listener bind must not contain surrounding whitespace")
	}
	if _, err := netip.ParseAddr(listener.Bind); err != nil {
		return errors.New("listener bind must be an IP address")
	}
	return nil
}

type ApplyError struct {
	Code      string `json:"code"`
	Path      string `json:"path,omitempty"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func (e *ApplyError) Error() string {
	if e == nil {
		return ""
	}
	if e.Path == "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s at %s: %s", e.Code, e.Path, e.Message)
}

// Validate performs structural validation and returns stable error fields
// suitable for both the REST API and the control protocol.
func (s DesiredSnapshot) Validate() error {
	if s.SchemaVersion != CurrentControlProtocolVersion {
		return &ApplyError{Code: "unknown_schema", Path: "schema_version", Message: fmt.Sprintf("schema version %d is unsupported", s.SchemaVersion)}
	}
	if err := ValidateID(s.NodeID, "node_id"); err != nil {
		return err
	}
	if s.Generation == 0 {
		return &ApplyError{Code: "invalid_generation", Path: "generation", Message: "generation must be positive"}
	}
	if len(s.Services) > maxSnapshotServices || len(s.Assignments) > maxSnapshotAssignments {
		return &ApplyError{Code: "snapshot_too_large", Message: "snapshot contains too many resources"}
	}
	if s.Gateway != nil && s.Agent != nil {
		return &ApplyError{Code: "invalid_node_spec", Message: "snapshot must contain at most one gateway or agent spec"}
	}
	// An empty snapshot is the explicit fail-closed state used after an
	// operator removes a NodeSpec. It carries no data-plane resources and lets
	// a connected generic Node retire its previous behavior without inventing a
	// second control-protocol message type.
	if s.Gateway == nil && s.Agent == nil && (len(s.Services) > 0 || len(s.Assignments) > 0) {
		return &ApplyError{Code: "invalid_node_spec", Message: "an unconfigured snapshot cannot contain services or assignments"}
	}
	if s.Gateway != nil {
		if s.Gateway.NodeID != s.NodeID {
			return &ApplyError{Code: "node_mismatch", Path: "gateway.node_id", Message: "gateway node id does not match snapshot"}
		}
		if err := s.Gateway.Validate(); err != nil {
			return err
		}
	}
	if s.Agent != nil {
		if s.Agent.NodeID != s.NodeID {
			return &ApplyError{Code: "node_mismatch", Path: "agent.node_id", Message: "agent node id does not match snapshot"}
		}
		if err := s.Agent.Validate(); err != nil {
			return err
		}
	}
	seen := make(map[string]struct{}, len(s.Services))
	serviceAgents := make(map[string]string, len(s.Services))
	serviceProtocols := make(map[string]string, len(s.Services))
	serviceBinds := make(map[string]string, len(s.Services))
	servicePorts := make(map[string]uint16, len(s.Services))
	for i := range s.Services {
		if err := s.Services[i].Validate(); err != nil {
			return &ApplyError{Code: "invalid_service", Path: fmt.Sprintf("services[%d]", i), Message: err.Error()}
		}
		if _, ok := seen[s.Services[i].ID]; ok {
			return &ApplyError{Code: "duplicate_service", Path: fmt.Sprintf("services[%d].id", i), Message: "service id is duplicated"}
		}
		seen[s.Services[i].ID] = struct{}{}
		serviceAgents[s.Services[i].ID] = s.Services[i].AgentID
		serviceProtocols[s.Services[i].ID] = s.Services[i].Protocol
		serviceBinds[s.Services[i].ID] = normalizedIP(s.Services[i].PublicBind)
		servicePorts[s.Services[i].ID] = s.Services[i].PublicPort
		if s.Agent != nil && s.Services[i].AgentID != s.NodeID {
			return &ApplyError{Code: "node_mismatch", Path: fmt.Sprintf("services[%d].agent_id", i), Message: "agent snapshot may only contain services owned by its node"}
		}
	}
	assignmentIDs := make(map[string]struct{}, len(s.Assignments))
	assignedServices := make(map[string]string, len(s.Services))
	assignedBindings := make(map[string]string)
	// Gateway listeners occupy the same public bind namespace as assignment
	// bindings. Seed the set before checking assignments so a service cannot
	// claim a port already reserved by a listener in the same generation.
	if s.Gateway != nil {
		for _, listener := range s.Gateway.Listeners {
			bind := normalizedIP(listener.Bind)
			assignedBindings[fmt.Sprintf("%s|%s|%d", listener.Protocol, bind, listener.Port)] = "listener"
		}
	}
	for i := range s.Assignments {
		if err := s.Assignments[i].Validate(); err != nil {
			return &ApplyError{Code: "invalid_assignment", Path: fmt.Sprintf("assignments[%d]", i), Message: err.Error()}
		}
		assignment := s.Assignments[i]
		if _, exists := assignmentIDs[assignment.ID]; exists {
			return &ApplyError{Code: "duplicate_assignment", Path: fmt.Sprintf("assignments[%d].id", i), Message: "assignment id is duplicated"}
		}
		assignmentIDs[assignment.ID] = struct{}{}
		// Assignment generations are shared by both ends of a data-plane
		// session, while snapshot generations are node-local. A Gateway-only
		// spec change may advance its snapshot without changing the assignment;
		// reject only an assignment that is newer than the snapshot carrying it.
		if assignment.Generation > s.Generation {
			return &ApplyError{Code: "generation_mismatch", Path: fmt.Sprintf("assignments[%d].generation", i), Message: "assignment generation cannot be newer than snapshot generation"}
		}
		if s.Gateway != nil && assignment.GatewayID != s.NodeID {
			return &ApplyError{Code: "node_mismatch", Path: fmt.Sprintf("assignments[%d].gateway_id", i), Message: "gateway snapshot contains an assignment for another gateway"}
		}
		if s.Agent != nil && assignment.AgentID != s.NodeID {
			return &ApplyError{Code: "node_mismatch", Path: fmt.Sprintf("assignments[%d].agent_id", i), Message: "agent snapshot contains an assignment for another agent"}
		}
		assignmentServices := make(map[string]struct{}, len(assignment.ServiceIDs))
		for _, serviceID := range assignment.ServiceIDs {
			serviceAgent, ok := serviceAgents[serviceID]
			if !ok {
				return &ApplyError{Code: "unknown_service", Path: fmt.Sprintf("assignments[%d].service_ids", i), Message: fmt.Sprintf("service %q is not present in snapshot", serviceID)}
			}
			if serviceAgent != assignment.AgentID {
				return &ApplyError{Code: "service_agent_mismatch", Path: fmt.Sprintf("assignments[%d].service_ids", i), Message: fmt.Sprintf("service %q belongs to another agent", serviceID)}
			}
			assignmentServices[serviceID] = struct{}{}
			if previous, exists := assignedServices[serviceID]; exists && previous != assignment.ID {
				return &ApplyError{Code: "resource_conflict", Path: fmt.Sprintf("assignments[%d].service_ids", i), Message: fmt.Sprintf("service %q is assigned more than once", serviceID)}
			}
			assignedServices[serviceID] = assignment.ID
		}
		for _, binding := range assignment.Bindings {
			if _, ok := assignmentServices[binding.ServiceID]; !ok {
				return &ApplyError{Code: "unknown_service", Path: fmt.Sprintf("assignments[%d].bindings", i), Message: fmt.Sprintf("binding references service %q outside service_ids", binding.ServiceID)}
			}
			if serviceProtocols[binding.ServiceID] != binding.Protocol {
				return &ApplyError{Code: "protocol_mismatch", Path: fmt.Sprintf("assignments[%d].bindings", i), Message: fmt.Sprintf("binding protocol for service %q does not match the service", binding.ServiceID)}
			}
			if serviceBinds[binding.ServiceID] != normalizedIP(binding.Bind) {
				return &ApplyError{Code: "bind_mismatch", Path: fmt.Sprintf("assignments[%d].bindings", i), Message: fmt.Sprintf("binding address for service %q does not match the service public bind", binding.ServiceID)}
			}
			if requestedPort := servicePorts[binding.ServiceID]; requestedPort != 0 && requestedPort != binding.Port {
				return &ApplyError{Code: "port_mismatch", Path: fmt.Sprintf("assignments[%d].bindings", i), Message: fmt.Sprintf("binding port for service %q does not match the service public port", binding.ServiceID)}
			}
			bind := strings.TrimSpace(binding.Bind)
			if address, parseErr := netip.ParseAddr(bind); parseErr == nil {
				bind = address.Unmap().String()
			}
			key := fmt.Sprintf("%s|%s|%d", binding.Protocol, bind, binding.Port)
			if previous, exists := assignedBindings[key]; exists && previous != assignment.ID {
				return &ApplyError{Code: "resource_conflict", Path: fmt.Sprintf("assignments[%d].bindings", i), Message: "public binding is duplicated across assignments"}
			}
			assignedBindings[key] = assignment.ID
		}
	}
	if s.Checksum != "" {
		checksum, err := s.ComputeChecksum()
		if err != nil {
			return &ApplyError{Code: "checksum_error", Message: err.Error()}
		}
		if !strings.EqualFold(checksum, s.Checksum) {
			return &ApplyError{Code: "checksum_mismatch", Path: "checksum", Message: "snapshot checksum does not match its content"}
		}
	}
	return nil
}

func (g GatewaySpec) Validate() error {
	if err := ValidateID(g.NodeID, "gateway.node_id"); err != nil {
		return err
	}
	if len(g.PublicEndpoints) == 0 {
		return &ApplyError{Code: "missing_endpoint", Path: "gateway.public_endpoints", Message: "at least one public endpoint is required"}
	}
	for i, endpoint := range g.PublicEndpoints {
		if err := validateEndpoint(endpoint); err != nil {
			return &ApplyError{Code: "invalid_endpoint", Path: fmt.Sprintf("gateway.public_endpoints[%d]", i), Message: err.Error()}
		}
	}
	if len(g.PublicEndpoints) > 64 {
		return &ApplyError{Code: "too_many_endpoints", Path: "gateway.public_endpoints", Message: "gateway has too many public endpoints"}
	}
	seenEndpoints := make(map[string]struct{}, len(g.PublicEndpoints))
	for _, endpoint := range g.PublicEndpoints {
		endpoint = strings.TrimSpace(endpoint)
		if _, exists := seenEndpoints[endpoint]; exists {
			return &ApplyError{Code: "duplicate_endpoint", Path: "gateway.public_endpoints", Message: "gateway public endpoint is duplicated"}
		}
		seenEndpoints[endpoint] = struct{}{}
	}
	if err := validateLabels(g.Labels, "gateway.labels"); err != nil {
		return err
	}
	if g.Revision < 0 {
		return &ApplyError{Code: "invalid_revision", Path: "gateway.revision", Message: "revision cannot be negative"}
	}
	if len(g.PortPool.TCP) > 1024 || len(g.PortPool.UDP) > 1024 {
		return &ApplyError{Code: "invalid_port_pool", Path: "gateway.port_pool", Message: "gateway has too many port ranges"}
	}
	seenListeners := make(map[string]struct{}, len(g.Listeners))
	if len(g.Listeners) > 4096 {
		return &ApplyError{Code: "too_many_listeners", Path: "gateway.listeners", Message: "gateway has too many listeners"}
	}
	for i, listener := range g.Listeners {
		if err := validateListener(listener); err != nil {
			return &ApplyError{Code: "invalid_listener", Path: fmt.Sprintf("gateway.listeners[%d]", i), Message: err.Error()}
		}
		bind := strings.TrimSpace(listener.Bind)
		if address, parseErr := netip.ParseAddr(bind); parseErr == nil {
			bind = address.Unmap().String()
		}
		key := fmt.Sprintf("%s|%s|%d", listener.Protocol, bind, listener.Port)
		if _, exists := seenListeners[key]; exists {
			return &ApplyError{Code: "duplicate_listener", Path: fmt.Sprintf("gateway.listeners[%d]", i), Message: "listener binding is duplicated"}
		}
		seenListeners[key] = struct{}{}
	}
	if g.Capacity.MaxAgents < 0 || g.Capacity.MaxServices < 0 || g.Capacity.MaxConnections < 0 {
		return &ApplyError{Code: "invalid_capacity", Path: "gateway.capacity", Message: "capacity values cannot be negative"}
	}
	if g.Capacity.MaxAgents > 1<<20 || g.Capacity.MaxServices > 1<<20 || g.Capacity.MaxConnections > 1<<20 {
		return &ApplyError{Code: "invalid_capacity", Path: "gateway.capacity", Message: "capacity values exceed the supported maximum"}
	}
	if err := g.PortPool.Validate(); err != nil {
		return err
	}
	if g.Transport.MaxStreams < 0 || g.Transport.MaxFrameBytes < 0 || g.Transport.MaxDatagramBytes < 0 || g.Transport.HandshakeTimeoutSeconds < 0 || g.Transport.IdleTimeoutSeconds < 0 {
		return &ApplyError{Code: "invalid_transport", Path: "gateway.transport", Message: "transport limits cannot be negative"}
	}
	if g.Transport.MaxStreams > 1<<20 || g.Transport.MaxFrameBytes > 1<<20 || g.Transport.MaxDatagramBytes > 64<<10 || g.Transport.HandshakeTimeoutSeconds > 24*60*60 || g.Transport.IdleTimeoutSeconds > 7*24*60*60 {
		return &ApplyError{Code: "invalid_transport", Path: "gateway.transport", Message: "transport limits exceed the supported maximum"}
	}
	if g.Transport.ALPN != "" && g.Transport.ALPN != DataALPN || len(g.Transport.ALPN) > 255 || containsControl(g.Transport.ALPN) {
		return &ApplyError{Code: "invalid_transport", Path: "gateway.transport.alpn", Message: "transport ALPN is invalid"}
	}
	if err := g.Obfuscation.Validate("gateway.obfuscation"); err != nil {
		return err
	}
	if err := g.Egress.Validate("gateway.egress"); err != nil {
		return err
	}
	return nil
}

func (a AgentSpec) Validate() error {
	if err := ValidateID(a.NodeID, "agent.node_id"); err != nil {
		return err
	}
	if a.Limits.MaxConnections < 0 || a.Limits.MaxStreams < 0 || a.Limits.MaxBufferBytes < 0 {
		return &ApplyError{Code: "invalid_limits", Path: "agent.limits", Message: "limit values cannot be negative"}
	}
	if a.Limits.MaxConnections > 1<<20 || a.Limits.MaxStreams > 1<<20 || a.Limits.MaxBufferBytes > 64<<20 {
		return &ApplyError{Code: "invalid_limits", Path: "agent.limits", Message: "agent limits exceed the supported maximum"}
	}
	if err := a.Egress.Validate("agent.egress"); err != nil {
		return err
	}
	if a.Revision < 0 {
		return &ApplyError{Code: "invalid_revision", Path: "agent.revision", Message: "revision cannot be negative"}
	}
	if err := a.GatewaySelector.Validate("agent.gateway_selector"); err != nil {
		return err
	}
	if len(a.Proxies) > 1024 || len(a.Routes) > 4096 {
		return &ApplyError{Code: "agent_spec_too_large", Path: "agent", Message: "agent spec contains too many entries"}
	}
	seenProxies := make(map[string]struct{}, len(a.Proxies))
	seenProxyBinds := make(map[string]struct{}, len(a.Proxies))
	for i, proxy := range a.Proxies {
		if err := ValidateID(proxy.ID, fmt.Sprintf("agent.proxies[%d].id", i)); err != nil || (proxy.Protocol != "http" && proxy.Protocol != "socks5") {
			return &ApplyError{Code: "invalid_proxy", Path: fmt.Sprintf("agent.proxies[%d]", i), Message: "proxy id and protocol are invalid"}
		}
		if _, exists := seenProxies[proxy.ID]; exists {
			return &ApplyError{Code: "duplicate_proxy", Path: fmt.Sprintf("agent.proxies[%d].id", i), Message: "proxy id is duplicated"}
		}
		seenProxies[proxy.ID] = struct{}{}
		if proxy.Bind == "" {
			return &ApplyError{Code: "invalid_proxy", Path: fmt.Sprintf("agent.proxies[%d].bind", i), Message: "proxy bind is required"}
		}
		if err := validateHostPort(proxy.Bind); err != nil {
			return &ApplyError{Code: "invalid_proxy", Path: fmt.Sprintf("agent.proxies[%d].bind", i), Message: "proxy bind must be host:port"}
		}
		// HTTP and SOCKS5 entrances cannot share one TCP socket even though
		// their protocol labels differ. Reserve the physical bind address as
		// the uniqueness key so the node can atomically recreate listeners.
		proxyBindKey := proxy.Bind
		if _, exists := seenProxyBinds[proxyBindKey]; exists {
			return &ApplyError{Code: "duplicate_proxy_bind", Path: fmt.Sprintf("agent.proxies[%d].bind", i), Message: "proxy bind is duplicated"}
		}
		seenProxyBinds[proxyBindKey] = struct{}{}
		if proxy.Route != "" && proxy.Route != "direct" && proxy.Route != "gateway" {
			return &ApplyError{Code: "invalid_proxy", Path: fmt.Sprintf("agent.proxies[%d].route", i), Message: "proxy route must be direct or gateway"}
		}
	}
	seenRoutes := make(map[string]struct{}, len(a.Routes))
	for i, route := range a.Routes {
		if err := ValidateID(route.Name, fmt.Sprintf("agent.routes[%d].name", i)); err != nil {
			return &ApplyError{Code: "invalid_route", Path: fmt.Sprintf("agent.routes[%d].name", i), Message: err.Error()}
		}
		if _, exists := seenRoutes[route.Name]; exists {
			return &ApplyError{Code: "duplicate_route", Path: fmt.Sprintf("agent.routes[%d].name", i), Message: "route name is duplicated"}
		}
		seenRoutes[route.Name] = struct{}{}
		if strings.TrimSpace(route.Destination) == "" || len(route.Destination) > 2048 || containsControl(route.Destination) {
			return &ApplyError{Code: "invalid_route", Path: fmt.Sprintf("agent.routes[%d].destination", i), Message: "route destination is invalid"}
		}
		if route.Destination != "direct" && route.Destination != "gateway" {
			return &ApplyError{Code: "invalid_route", Path: fmt.Sprintf("agent.routes[%d].destination", i), Message: "route destination must be direct or gateway"}
		}
		if len(route.CIDRs) > 1024 || len(route.Domains) > 1024 || len(route.GeoIP) > 256 {
			return &ApplyError{Code: "invalid_route", Path: fmt.Sprintf("agent.routes[%d]", i), Message: "route contains too many match entries"}
		}
		for _, cidr := range route.CIDRs {
			if _, err := netip.ParsePrefix(strings.TrimSpace(cidr)); err != nil {
				return &ApplyError{Code: "invalid_route", Path: fmt.Sprintf("agent.routes[%d].cidrs", i), Message: "route CIDR is invalid"}
			}
		}
		for _, domainName := range route.Domains {
			if strings.TrimSpace(domainName) == "" || strings.TrimSpace(domainName) != domainName || len(domainName) > 253 || containsControl(domainName) || strings.ContainsAny(domainName, " \t") {
				return &ApplyError{Code: "invalid_route", Path: fmt.Sprintf("agent.routes[%d].domains", i), Message: "route domain is invalid"}
			}
		}
		for _, country := range route.GeoIP {
			country = strings.TrimSpace(country)
			if country != strings.ToLower(country) || country == "" || len(country) > 16 || country != strings.TrimSpace(country) || containsControl(country) {
				return &ApplyError{Code: "invalid_route", Path: fmt.Sprintf("agent.routes[%d].geoip", i), Message: "route GeoIP code is invalid"}
			}
			if country != "private" && (len(country) != 2 || country[0] < 'a' || country[0] > 'z' || country[1] < 'a' || country[1] > 'z') {
				return &ApplyError{Code: "invalid_route", Path: fmt.Sprintf("agent.routes[%d].geoip", i), Message: "route GeoIP code must be private or a two-letter ISO code"}
			}
		}
	}
	return nil
}

func validateLabels(labels map[string]string, path string) error {
	if len(labels) > 128 {
		return &ApplyError{Code: "invalid_labels", Path: path, Message: "too many labels"}
	}
	for key, value := range labels {
		if strings.TrimSpace(key) == "" || strings.TrimSpace(key) != key || len(key) > 128 || containsControl(key) || len(value) > 256 || containsControl(value) {
			return &ApplyError{Code: "invalid_labels", Path: path, Message: "label is invalid"}
		}
	}
	return nil
}

func (s Service) Validate() error {
	if err := ValidateID(s.ID, "service.id"); err != nil {
		return err
	}
	if err := ValidateID(s.AgentID, "service.agent_id"); err != nil {
		return err
	}
	if s.Revision < 0 {
		return &ApplyError{Code: "invalid_revision", Path: "service.revision", Message: "revision cannot be negative"}
	}
	if s.Protocol != ProtocolTCP && s.Protocol != ProtocolUDP {
		return errors.New("service protocol must be tcp or udp")
	}
	if err := s.GatewaySelector.Validate("service.gateway_selector"); err != nil {
		return err
	}
	if strings.TrimSpace(s.LocalTarget) == "" {
		return errors.New("service local_target is required")
	}
	if len(s.LocalTarget) > 2048 || containsControl(s.LocalTarget) {
		return errors.New("service local_target is invalid")
	}
	if strings.TrimSpace(s.PublicBind) == "" {
		return errors.New("service public_bind is required")
	}
	if s.PublicBind != strings.TrimSpace(s.PublicBind) {
		return errors.New("service public_bind must not contain surrounding whitespace")
	}
	if _, err := netip.ParseAddr(s.PublicBind); err != nil {
		return errors.New("service public_bind must be an IP address")
	}
	// A zero public port requests allocation from the selected Gateway's
	// protocol-specific port pool. Explicit ports are checked by the scheduler
	// inside the same transaction that records the assignment.
	if s.PublicPort != 0 {
		if _, err := net.ResolveTCPAddr("tcp", net.JoinHostPort(s.PublicBind, fmt.Sprint(s.PublicPort))); err != nil && s.Protocol == ProtocolTCP {
			return fmt.Errorf("service public bind: %w", err)
		}
	}
	if err := validateHostPort(s.LocalTarget); err != nil {
		return fmt.Errorf("service local_target must be host:port: %w", err)
	}
	return nil
}

func (a Assignment) Validate() error {
	if err := ValidateID(a.ID, "assignment.id"); err != nil {
		return err
	}
	if err := ValidateID(a.GatewayID, "assignment.gateway_id"); err != nil {
		return err
	}
	if err := ValidateID(a.AgentID, "assignment.agent_id"); err != nil {
		return err
	}
	if a.Generation == 0 {
		return errors.New("assignment generation must be positive")
	}
	if a.State != "" && a.State != AssignmentPending && a.State != AssignmentApplied && a.State != AssignmentDegraded && a.State != AssignmentDraining {
		return &ApplyError{Code: "invalid_assignment_state", Path: "assignment.state", Message: "assignment state is invalid"}
	}
	if a.PublicEndpoint != "" {
		if err := validateEndpoint(a.PublicEndpoint); err != nil {
			return &ApplyError{Code: "invalid_endpoint", Path: "assignment.public_endpoint", Message: err.Error()}
		}
	}
	if err := a.Obfuscation.Validate("assignment.obfuscation"); err != nil {
		return err
	}
	if len(a.ServiceIDs) > maxAssignmentServices || len(a.Bindings) > maxAssignmentBindings {
		return &ApplyError{Code: "assignment_too_large", Message: "assignment contains too many services or bindings"}
	}
	serviceIDs := make(map[string]struct{}, len(a.ServiceIDs))
	for _, serviceID := range a.ServiceIDs {
		if err := ValidateID(serviceID, "assignment.service_ids"); err != nil {
			return err
		}
		if _, ok := serviceIDs[serviceID]; ok {
			return errors.New("assignment contains duplicate service ids")
		}
		serviceIDs[serviceID] = struct{}{}
	}
	seen := make(map[string]struct{}, len(a.Bindings))
	seenBindingServices := make(map[string]struct{}, len(a.Bindings))
	for _, binding := range a.Bindings {
		if err := ValidateID(binding.ServiceID, "assignment.binding.service_id"); err != nil {
			return err
		}
		if binding.Protocol != ProtocolTCP && binding.Protocol != ProtocolUDP {
			return errors.New("assignment binding protocol must be tcp or udp")
		}
		if binding.Port == 0 {
			return errors.New("assignment binding port must be non-zero")
		}
		if binding.Bind != strings.TrimSpace(binding.Bind) {
			return errors.New("assignment binding bind must not contain surrounding whitespace")
		}
		if _, err := netip.ParseAddr(binding.Bind); err != nil {
			return errors.New("assignment binding bind must be an IP address")
		}
		if _, exists := seenBindingServices[binding.ServiceID]; exists {
			return errors.New("assignment contains multiple bindings for one service")
		}
		seenBindingServices[binding.ServiceID] = struct{}{}
		bind := strings.TrimSpace(binding.Bind)
		if address, err := netip.ParseAddr(bind); err == nil {
			bind = address.Unmap().String()
		}
		key := fmt.Sprintf("%s|%s|%d", binding.Protocol, bind, binding.Port)
		if _, ok := seen[key]; ok {
			return errors.New("assignment contains duplicate public bindings")
		}
		seen[key] = struct{}{}
	}
	return nil
}
