package cloud

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/nexu-io/looper/internal/network/protocol"
	"github.com/nexu-io/looper/internal/storage"
)

var errUnauthorized = errors.New("unauthorized")
var randRead = rand.Read

const leaseRevalidationProbeTimeout = 2 * time.Second

type Service struct {
	config  Config
	db      *sql.DB
	now     func() time.Time
	leaseMu sync.Mutex
	mu      sync.Mutex
	subs    map[chan protocol.AuditEnvelope]struct{}
}

func Open(ctx context.Context, cfg Config) (*Service, error) {
	db, err := sql.Open(storage.DriverName, cfg.DBPath)
	if err != nil {
		return nil, err
	}
	service := &Service{config: cfg, db: db, now: time.Now, subs: map[chan protocol.AuditEnvelope]struct{}{}}
	if err := service.init(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return service, nil
}

func (s *Service) Close() error { return s.db.Close() }

func (s *Service) init(ctx context.Context) error {
	stmts := []string{
		`PRAGMA journal_mode=WAL;`,
		`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);`,
		`CREATE TABLE IF NOT EXISTS join_keys (join_key TEXT PRIMARY KEY, created_at TEXT NOT NULL, consumed_at TEXT, consumed_by_node_id TEXT);`,
		`CREATE TABLE IF NOT EXISTS nodes (node_id TEXT PRIMARY KEY, node_name TEXT NOT NULL UNIQUE COLLATE NOCASE, node_token TEXT NOT NULL UNIQUE, daemon_version TEXT NOT NULL, github_numeric_id INTEGER NOT NULL, github_login TEXT NOT NULL, target_labels TEXT NOT NULL, capabilities_json TEXT NOT NULL DEFAULT '{}', joined_at TEXT NOT NULL, last_heartbeat_at TEXT, active INTEGER NOT NULL DEFAULT 1);`,
		`CREATE TABLE IF NOT EXISTS coordinator_leases (name TEXT PRIMARY KEY, holder_node_id TEXT, fencing_token INTEGER NOT NULL, expires_at TEXT);`,
		`ALTER TABLE nodes ADD COLUMN capabilities_json TEXT NOT NULL DEFAULT '{}'`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return err
		}
	}
	return s.ensureNetworkID(ctx)
}

func (s *Service) ensureNetworkID(ctx context.Context) error {
	var existing string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'network_id'`).Scan(&existing)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	networkID := s.config.NetworkID
	if networkID == "" {
		token, err := randomToken(8)
		if err != nil {
			return err
		}
		networkID = "net_" + token
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES('network_id', ?)`, networkID)
	return err
}

func (s *Service) NetworkID(ctx context.Context) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'network_id'`).Scan(&id)
	return id, err
}

func (s *Service) CreateJoinKey(ctx context.Context) (string, error) {
	token, err := randomToken(12)
	if err != nil {
		return "", err
	}
	key := "join_" + token
	_, err = s.db.ExecContext(ctx, `INSERT INTO join_keys(join_key, created_at) VALUES(?, ?)`, key, s.now().UTC().Format(time.RFC3339Nano))
	return key, err
}

func (s *Service) Join(ctx context.Context, req protocol.JoinRequest) (protocol.JoinResponse, error) {
	if err := protocol.ValidateCompatibility(req.ProtocolVersion, req.DaemonVersion, s.config.MinimumDaemonVersion); err != nil {
		return protocol.JoinResponse{}, err
	}
	if err := protocol.ValidateNodeName(req.NodeName); err != nil {
		return protocol.JoinResponse{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return protocol.JoinResponse{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE join_keys SET consumed_at = ?, consumed_by_node_id = ? WHERE join_key = ? AND consumed_at IS NULL`, s.now().UTC().Format(time.RFC3339Nano), "pending", req.JoinKey)
	if err != nil {
		return protocol.JoinResponse{}, err
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return protocol.JoinResponse{}, fmt.Errorf("join key is invalid or already consumed")
	}
	nodeIDToken, err := randomToken(8)
	if err != nil {
		return protocol.JoinResponse{}, err
	}
	nodeTokenValue, err := randomToken(16)
	if err != nil {
		return protocol.JoinResponse{}, err
	}
	nodeID := "node_" + nodeIDToken
	nodeToken := "node_" + nodeTokenValue
	labelsJSON, err := json.Marshal(targetLabelsForJoin(req.NodeName, req.TargetLabels))
	if err != nil {
		return protocol.JoinResponse{}, err
	}
	joinedAt := s.now().UTC().Format(time.RFC3339Nano)
	result, err = tx.ExecContext(ctx, `UPDATE nodes SET node_token = ?, daemon_version = ?, github_numeric_id = ?, github_login = ?, target_labels = ?, capabilities_json = '{}', joined_at = ?, last_heartbeat_at = NULL, active = 1 WHERE node_name = ? AND active = 0`, nodeToken, req.DaemonVersion, req.GitHub.NumericID, req.GitHub.Login, string(labelsJSON), joinedAt, req.NodeName)
	if err != nil {
		return protocol.JoinResponse{}, err
	}
	rows, _ = result.RowsAffected()
	if rows == 1 {
		if err := tx.QueryRowContext(ctx, `SELECT node_id FROM nodes WHERE node_name = ?`, req.NodeName).Scan(&nodeID); err != nil {
			return protocol.JoinResponse{}, err
		}
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO nodes(node_id, node_name, node_token, daemon_version, github_numeric_id, github_login, target_labels, capabilities_json, joined_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`, nodeID, req.NodeName, nodeToken, req.DaemonVersion, req.GitHub.NumericID, req.GitHub.Login, string(labelsJSON), `{}`, joinedAt)
	}
	if err != nil {
		var sqliteErr sqlite3.Error
		if errors.As(err, &sqliteErr) && sqliteErr.ExtendedCode == sqlite3.ErrConstraintUnique {
			return protocol.JoinResponse{}, fmt.Errorf("node name %q is already active", req.NodeName)
		}
		return protocol.JoinResponse{}, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE join_keys SET consumed_by_node_id = ? WHERE join_key = ?`, nodeID, req.JoinKey)
	if err != nil {
		return protocol.JoinResponse{}, err
	}
	if err := tx.Commit(); err != nil {
		return protocol.JoinResponse{}, err
	}
	networkID, err := s.NetworkID(ctx)
	if err != nil {
		return protocol.JoinResponse{}, err
	}
	warnings, err := s.duplicateWarnings(ctx)
	if err != nil {
		return protocol.JoinResponse{}, err
	}
	return protocol.JoinResponse{NetworkID: networkID, NodeID: nodeID, NodeToken: nodeToken, Warnings: warningsForNumericID(warnings, req.GitHub.NumericID)}, nil
}

func (s *Service) Heartbeat(ctx context.Context, nodeToken string, req protocol.HeartbeatRequest) (protocol.HeartbeatResponse, error) {
	if err := protocol.ValidateCompatibility(req.ProtocolVersion, req.DaemonVersion, s.config.MinimumDaemonVersion); err != nil {
		return protocol.HeartbeatResponse{}, err
	}
	nodeID, _, err := s.authenticateNode(ctx, nodeToken)
	if err != nil {
		return protocol.HeartbeatResponse{}, err
	}
	now := s.now().UTC()
	capabilitiesJSON, _ := json.Marshal(req.Capabilities)
	_, err = s.db.ExecContext(ctx, `UPDATE nodes SET last_heartbeat_at = ?, daemon_version = ?, capabilities_json = ? WHERE node_id = ?`, now.Format(time.RFC3339Nano), req.DaemonVersion, string(capabilitiesJSON), nodeID)
	if err != nil {
		return protocol.HeartbeatResponse{}, err
	}
	warnings, err := s.duplicateWarnings(ctx)
	if err != nil {
		return protocol.HeartbeatResponse{}, err
	}
	return protocol.HeartbeatResponse{RecordedAt: now, Warnings: warnings}, nil
}

func (s *Service) Leave(ctx context.Context, nodeToken string) error {
	nodeID, _, err := s.authenticateNode(ctx, nodeToken)
	if err != nil {
		return err
	}
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := s.currentLeaseTx(ctx, tx)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE nodes SET active = 0 WHERE node_id = ?`, nodeID); err != nil {
		return err
	}
	if current.HolderNodeID == nodeID {
		if _, err := tx.ExecContext(ctx, `INSERT INTO coordinator_leases(name, holder_node_id, fencing_token, expires_at) VALUES(?, ?, ?, ?) ON CONFLICT(name) DO UPDATE SET holder_node_id=excluded.holder_node_id, fencing_token=excluded.fencing_token, expires_at=excluded.expires_at`, protocol.DefaultLeaseName, nil, current.FencingToken+1, nil); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if current.HolderNodeID == nodeID {
		s.publish(protocol.AuditEnvelope{Event: "lease.changed", LeaseName: protocol.DefaultLeaseName, LeaseToken: current.FencingToken + 1, OccurredAt: s.now().UTC()})
	}
	return nil
}

func (s *Service) AcquireLease(ctx context.Context, nodeToken string, _ int) (protocol.CoordinatorLease, error) {
	nodeID, _, err := s.authenticateNode(ctx, nodeToken)
	if err != nil {
		return protocol.CoordinatorLease{}, err
	}
	return s.mutateLease(ctx, func(tx *sql.Tx, current protocol.CoordinatorLease) (protocol.CoordinatorLease, error) {
		now := s.now().UTC()
		if current.HolderNodeID != "" && current.ExpiresAt != nil && current.ExpiresAt.After(now) {
			return protocol.CoordinatorLease{}, fmt.Errorf("coordinator lease is already held by %s", current.HolderNodeID)
		}
		expires := now.Add(s.leaseTTL())
		lease := protocol.CoordinatorLease{Name: protocol.DefaultLeaseName, HolderNodeID: nodeID, FencingToken: current.FencingToken + 1, ExpiresAt: &expires}
		return lease, nil
	})
}

func (s *Service) RenewLease(ctx context.Context, nodeToken string, fencingToken int64, _ int) (protocol.CoordinatorLease, error) {
	nodeID, _, err := s.authenticateNode(ctx, nodeToken)
	if err != nil {
		return protocol.CoordinatorLease{}, err
	}
	return s.mutateLease(ctx, func(tx *sql.Tx, current protocol.CoordinatorLease) (protocol.CoordinatorLease, error) {
		if !leaseUsable(current, s.now().UTC()) || current.HolderNodeID != nodeID || current.FencingToken != fencingToken {
			return protocol.CoordinatorLease{}, staleLeaseError(current)
		}
		expires := s.now().UTC().Add(s.leaseTTL())
		current.ExpiresAt = &expires
		return current, nil
	})
}

func (s *Service) HandoffLease(ctx context.Context, nodeToken string, fencingToken int64, targetNodeName string, _ int) (protocol.CoordinatorLease, error) {
	ownerID, _, err := s.authenticateNode(ctx, nodeToken)
	if err != nil {
		return protocol.CoordinatorLease{}, err
	}
	return s.mutateLease(ctx, func(tx *sql.Tx, current protocol.CoordinatorLease) (protocol.CoordinatorLease, error) {
		if !leaseUsable(current, s.now().UTC()) || current.HolderNodeID != ownerID || current.FencingToken != fencingToken {
			return protocol.CoordinatorLease{}, staleLeaseError(current)
		}
		var targetNodeID string
		if err := tx.QueryRowContext(ctx, `SELECT node_id FROM nodes WHERE node_name = ? AND active = 1`, targetNodeName).Scan(&targetNodeID); err != nil {
			return protocol.CoordinatorLease{}, fmt.Errorf("target node %q not found", targetNodeName)
		}
		expires := s.now().UTC().Add(s.leaseTTL())
		return protocol.CoordinatorLease{Name: protocol.DefaultLeaseName, HolderNodeID: targetNodeID, FencingToken: current.FencingToken + 1, ExpiresAt: &expires}, nil
	})
}

func (s *Service) ExpireLease(ctx context.Context, nodeToken string, fencingToken int64) (protocol.CoordinatorLease, error) {
	nodeID, _, err := s.authenticateNode(ctx, nodeToken)
	if err != nil {
		return protocol.CoordinatorLease{}, err
	}
	return s.mutateLease(ctx, func(tx *sql.Tx, current protocol.CoordinatorLease) (protocol.CoordinatorLease, error) {
		if !leaseUsable(current, s.now().UTC()) || current.HolderNodeID != nodeID || current.FencingToken != fencingToken {
			return protocol.CoordinatorLease{}, staleLeaseError(current)
		}
		return protocol.CoordinatorLease{Name: protocol.DefaultLeaseName, FencingToken: current.FencingToken + 1}, nil
	})
}

func (s *Service) RevalidateLease(ctx context.Context, nodeToken string, req protocol.CoordinatorLeaseRevalidateRequest) error {
	nodeID, _, err := s.authenticateNode(ctx, nodeToken)
	if err != nil {
		return err
	}
	lease, err := s.currentLease(ctx)
	if err != nil {
		return err
	}
	if !leaseUsable(lease, s.now().UTC()) || lease.HolderNodeID != nodeID || lease.FencingToken != req.FencingToken {
		return staleLeaseError(lease)
	}
	if err := s.probeLeaseTarget(ctx, lease, req); err != nil {
		return err
	}
	lease, err = s.currentLease(ctx)
	if err != nil {
		return err
	}
	if !leaseUsable(lease, s.now().UTC()) || lease.HolderNodeID != nodeID || lease.FencingToken != req.FencingToken {
		return staleLeaseError(lease)
	}
	return nil
}

func (s *Service) probeLeaseTarget(ctx context.Context, lease protocol.CoordinatorLease, req protocol.CoordinatorLeaseRevalidateRequest) error {
	method := strings.TrimSpace(req.Method)
	if method == "" {
		method = http.MethodGet
	}
	probeCtx, cancel := context.WithTimeout(ctx, leaseRevalidationProbeTimeout)
	defer cancel()
	probe, err := http.NewRequestWithContext(probeCtx, method, strings.TrimSpace(req.URL), nil)
	if err != nil {
		return fmt.Errorf("build revalidation probe: %w", err)
	}
	probe.Header.Set("X-Looper-Coordinator-Fencing-Token", fmt.Sprintf("%d", lease.FencingToken))
	resp, err := (&http.Client{Timeout: leaseRevalidationProbeTimeout, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}).Do(probe)
	if err != nil {
		return fmt.Errorf("%w: revalidation probe failed: %v", staleLeaseError(lease), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%w: revalidation probe returned status %d", staleLeaseError(lease), resp.StatusCode)
	}
	return nil
}

func (s *Service) Status(ctx context.Context) (protocol.StatusResponse, error) {
	networkID, err := s.NetworkID(ctx)
	if err != nil {
		return protocol.StatusResponse{}, err
	}
	lease, err := s.currentLease(ctx)
	if err != nil {
		return protocol.StatusResponse{}, err
	}
	members, err := s.memberships(ctx)
	if err != nil {
		return protocol.StatusResponse{}, err
	}
	warnings, err := s.duplicateWarnings(ctx)
	if err != nil {
		return protocol.StatusResponse{}, err
	}
	return protocol.StatusResponse{NetworkID: networkID, Lease: lease, Memberships: members, Warnings: warnings}, nil
}

func (s *Service) NodeStatus(ctx context.Context, nodeToken string) (protocol.NodeStatusResponse, error) {
	nodeID, member, err := s.authenticateNode(ctx, nodeToken)
	if err != nil {
		return protocol.NodeStatusResponse{}, err
	}
	members, err := s.memberships(ctx)
	if err != nil {
		return protocol.NodeStatusResponse{}, err
	}
	networkID, err := s.NetworkID(ctx)
	if err != nil {
		return protocol.NodeStatusResponse{}, err
	}
	lease, err := s.currentLease(ctx)
	if err != nil {
		return protocol.NodeStatusResponse{}, err
	}
	warnings, err := s.duplicateWarnings(ctx)
	if err != nil {
		return protocol.NodeStatusResponse{}, err
	}
	member.NodeID = nodeID
	return protocol.NodeStatusResponse{NetworkID: networkID, Membership: member, Memberships: members, Lease: lease, Warnings: warnings, CloudReachable: true, CurrentGitHub: member.GitHub, IdentityDrift: member.Capabilities.IdentityDrift, IdentityDriftReason: member.Capabilities.DriftReason}, nil
}

func (s *Service) currentLease(ctx context.Context) (protocol.CoordinatorLease, error) {
	var holder sql.NullString
	var token int64
	var expires sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT holder_node_id, fencing_token, expires_at FROM coordinator_leases WHERE name = ?`, protocol.DefaultLeaseName).Scan(&holder, &token, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return protocol.CoordinatorLease{Name: protocol.DefaultLeaseName}, nil
	}
	if err != nil {
		return protocol.CoordinatorLease{}, err
	}
	lease := protocol.CoordinatorLease{Name: protocol.DefaultLeaseName, FencingToken: token}
	if holder.Valid {
		lease.HolderNodeID = holder.String
	}
	if expires.Valid {
		t, err := time.Parse(time.RFC3339Nano, expires.String)
		if err != nil {
			return protocol.CoordinatorLease{}, err
		}
		lease.ExpiresAt = &t
	}
	return lease, nil
}

func (s *Service) memberships(ctx context.Context) ([]protocol.Membership, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT node_id, node_name, github_numeric_id, github_login, target_labels, capabilities_json, joined_at, last_heartbeat_at FROM nodes WHERE active = 1 ORDER BY node_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	warnMap, err := s.duplicateWarningMap(ctx)
	if err != nil {
		return nil, err
	}
	var out []protocol.Membership
	for rows.Next() {
		var m protocol.Membership
		var labelsJSON, capabilitiesJSON, joinedAt string
		var last sql.NullString
		if err := rows.Scan(&m.NodeID, &m.NodeName, &m.GitHub.NumericID, &m.GitHub.Login, &labelsJSON, &capabilitiesJSON, &joinedAt, &last); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(labelsJSON), &m.TargetLabels)
		_ = json.Unmarshal([]byte(capabilitiesJSON), &m.Capabilities)
		m.JoinedAt, _ = time.Parse(time.RFC3339Nano, joinedAt)
		if last.Valid {
			t, _ := time.Parse(time.RFC3339Nano, last.String)
			m.LastHeartbeatAt = &t
		}
		m.DuplicateWarning = warnMap[m.GitHub.NumericID]
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Service) authenticateNode(ctx context.Context, nodeToken string) (string, protocol.Membership, error) {
	if strings.TrimSpace(nodeToken) == "" {
		return "", protocol.Membership{}, errUnauthorized
	}
	var nodeID, nodeName, githubLogin, labelsJSON, capabilitiesJSON, joinedAt string
	var githubNumericID int64
	var last sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT node_id, node_name, github_numeric_id, github_login, target_labels, capabilities_json, joined_at, last_heartbeat_at FROM nodes WHERE node_token = ? AND active = 1`, nodeToken).Scan(&nodeID, &nodeName, &githubNumericID, &githubLogin, &labelsJSON, &capabilitiesJSON, &joinedAt, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return "", protocol.Membership{}, errUnauthorized
	}
	if err != nil {
		return "", protocol.Membership{}, err
	}
	member := protocol.Membership{NodeID: nodeID, NodeName: nodeName, GitHub: protocol.GitHubIdentity{NumericID: githubNumericID, Login: githubLogin}}
	_ = json.Unmarshal([]byte(labelsJSON), &member.TargetLabels)
	_ = json.Unmarshal([]byte(capabilitiesJSON), &member.Capabilities)
	member.JoinedAt, _ = time.Parse(time.RFC3339Nano, joinedAt)
	if last.Valid {
		t, _ := time.Parse(time.RFC3339Nano, last.String)
		member.LastHeartbeatAt = &t
	}
	return nodeID, member, nil
}

func (s *Service) duplicateWarnings(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT github_numeric_id, COUNT(*) FROM nodes WHERE active = 1 AND github_numeric_id > 0 GROUP BY github_numeric_id HAVING COUNT(*) > 1 ORDER BY github_numeric_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var warnings []string
	for rows.Next() {
		var id, count int64
		if err := rows.Scan(&id, &count); err != nil {
			return nil, err
		}
		warnings = append(warnings, fmt.Sprintf("duplicate active GitHub numeric identity %d is attached to %d nodes", id, count))
	}
	return warnings, rows.Err()
}

func (s *Service) duplicateWarningMap(ctx context.Context) (map[int64]bool, error) {
	warnings, err := s.duplicateWarnings(ctx)
	if err != nil {
		return nil, err
	}
	out := map[int64]bool{}
	for _, warning := range warnings {
		var id int64
		if _, err := fmt.Sscanf(warning, "duplicate active GitHub numeric identity %d", &id); err == nil {
			out[id] = true
		}
	}
	return out, nil
}

func warningsForNumericID(warnings []string, numericID int64) []string {
	needle := fmt.Sprintf("duplicate active GitHub numeric identity %d", numericID)
	var out []string
	for _, warning := range warnings {
		if strings.Contains(warning, needle) {
			out = append(out, warning)
		}
	}
	return out
}

func (s *Service) mutateLease(ctx context.Context, mutate func(*sql.Tx, protocol.CoordinatorLease) (protocol.CoordinatorLease, error)) (protocol.CoordinatorLease, error) {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return protocol.CoordinatorLease{}, err
	}
	defer tx.Rollback()
	current, err := s.currentLeaseTx(ctx, tx)
	if err != nil {
		return protocol.CoordinatorLease{}, err
	}
	next, err := mutate(tx, current)
	if err != nil {
		return protocol.CoordinatorLease{}, err
	}
	var expires any
	if next.ExpiresAt != nil {
		expires = next.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO coordinator_leases(name, holder_node_id, fencing_token, expires_at) VALUES(?, ?, ?, ?) ON CONFLICT(name) DO UPDATE SET holder_node_id=excluded.holder_node_id, fencing_token=excluded.fencing_token, expires_at=excluded.expires_at`, protocol.DefaultLeaseName, nullableString(next.HolderNodeID), next.FencingToken, expires)
	if err != nil {
		return protocol.CoordinatorLease{}, err
	}
	if err := tx.Commit(); err != nil {
		return protocol.CoordinatorLease{}, err
	}
	s.publish(protocol.AuditEnvelope{Event: "lease.changed", LeaseName: protocol.DefaultLeaseName, LeaseToken: next.FencingToken, NodeID: next.HolderNodeID, OccurredAt: s.now().UTC()})
	return next, nil
}

func (s *Service) currentLeaseTx(ctx context.Context, tx *sql.Tx) (protocol.CoordinatorLease, error) {
	var holder sql.NullString
	var token sql.NullInt64
	var expires sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT holder_node_id, fencing_token, expires_at FROM coordinator_leases WHERE name = ?`, protocol.DefaultLeaseName).Scan(&holder, &token, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return protocol.CoordinatorLease{Name: protocol.DefaultLeaseName}, nil
	}
	if err != nil {
		return protocol.CoordinatorLease{}, err
	}
	lease := protocol.CoordinatorLease{Name: protocol.DefaultLeaseName, FencingToken: token.Int64}
	if holder.Valid {
		lease.HolderNodeID = holder.String
	}
	if expires.Valid {
		t, err := time.Parse(time.RFC3339Nano, expires.String)
		if err != nil {
			return protocol.CoordinatorLease{}, err
		}
		lease.ExpiresAt = &t
	}
	return lease, nil
}

func staleLeaseError(current protocol.CoordinatorLease) error {
	return fmt.Errorf("stale coordinator lease token; current token is %d", current.FencingToken)
}

func leaseUsable(current protocol.CoordinatorLease, now time.Time) bool {
	return current.ExpiresAt != nil && current.ExpiresAt.After(now)
}

func (s *Service) leaseTTL() time.Duration {
	if s.config.LeaseTTLSeconds > 0 {
		return time.Duration(s.config.LeaseTTLSeconds) * time.Second
	}
	return protocol.DefaultLeaseTTL
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func ttlDuration(ttlSeconds int) time.Duration {
	if ttlSeconds <= 0 {
		ttlSeconds = int(protocol.DefaultLeaseTTL / time.Second)
	}
	return time.Duration(ttlSeconds) * time.Second
}

func targetLabelsForJoin(nodeName string, requested []string) []string {
	if len(requested) == 0 {
		return []string{protocol.TargetLabelForNode(nodeName)}
	}
	return append([]string(nil), requested...)
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := randRead(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (s *Service) Subscribe() (<-chan protocol.AuditEnvelope, func()) {
	ch := make(chan protocol.AuditEnvelope, 8)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.subs, ch)
		close(ch)
		s.mu.Unlock()
	}
}

func (s *Service) publish(event protocol.AuditEnvelope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- event:
		default:
		}
	}
}
