package controller

import (
	"context"
	"database/sql"
	"fmt"

	"asterferry/internal/domain"
)

// Service persistence is deliberately kept separate from the node aggregate
// codecs. Its only child table is the selector map, so its consistency rules
// remain easy to audit.
func loadServiceNormalized(ctx context.Context, q sqlQueryer, id string) (domain.Service, error) {
	var service domain.Service
	var publicPort, enabled int64
	var revision int64
	var updated string
	var err error
	if err := q.QueryRowContext(ctx, `SELECT id,agent_id,protocol,local_target,public_bind,public_port,enabled,revision,updated_at FROM services WHERE id=?`, id).Scan(&service.ID, &service.AgentID, &service.Protocol, &service.LocalTarget, &service.PublicBind, &publicPort, &enabled, &revision, &updated); err != nil {
		return domain.Service{}, err
	}
	service.PublicPort, err = storedPort(publicPort, "service public port")
	if err != nil {
		return domain.Service{}, err
	}
	service.Enabled = enabled != 0
	service.Revision = revision
	service.UpdatedAt, err = parseStoredTime("service.updated_at", updated)
	if err != nil {
		return domain.Service{}, err
	}
	service.GatewaySelector.MatchLabels, err = loadStringMap(ctx, q, "service_selector_labels", "service_id", id)
	if err != nil {
		return domain.Service{}, err
	}
	if err := service.Validate(); err != nil {
		return domain.Service{}, fmt.Errorf("stored service is invalid: %w", err)
	}
	return service, nil
}

func replaceServiceSelectorTx(ctx context.Context, tx *sql.Tx, serviceID string, selector domain.Selector) error {
	return replaceStringMapTx(ctx, tx, "service_selector_labels", "service_id", serviceID, selector.MatchLabels)
}

func nullableBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}
