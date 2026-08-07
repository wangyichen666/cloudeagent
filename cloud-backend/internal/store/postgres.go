package store

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"cloude-agent/internal/models"
)

//go:embed schema.sql
var postgresSchema string

// Postgres 是生产路径的元数据存储实现。
// 模型配置与凭证不经过它（文档 6.2：凭证不落盘）。
type Postgres struct {
	pool *pgxpool.Pool
}

func NewPostgres(ctx context.Context, dsn string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if _, err := pool.Exec(ctx, postgresSchema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Postgres{pool: pool}, nil
}

func (p *Postgres) CreateInstance(ctx context.Context, inst *models.Instance) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO agent_instances (user_id, instance_name, status, workspace, endpoint, port, error, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		inst.UserID, inst.InstanceName, string(inst.Status), inst.Workspace, inst.Endpoint, inst.Port, inst.Error,
		inst.CreatedAt, inst.UpdatedAt)
	return err
}

func (p *Postgres) GetInstance(ctx context.Context, userID string) (*models.Instance, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT user_id, instance_name, status, workspace, endpoint, port, error, created_at, updated_at
		FROM agent_instances WHERE user_id=$1`, userID)
	inst, err := scanInstance(row)
	if err == pgx.ErrNoRows {
		return nil, ErrNotFound
	}
	return inst, err
}

func (p *Postgres) ListInstances(ctx context.Context) ([]*models.Instance, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT user_id, instance_name, status, workspace, endpoint, port, error, created_at, updated_at
		FROM agent_instances ORDER BY user_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*models.Instance, 0)
	for rows.Next() {
		inst, err := scanInstance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inst)
	}
	return out, rows.Err()
}

func (p *Postgres) UpdateInstance(ctx context.Context, inst *models.Instance) error {
	tag, err := p.pool.Exec(ctx, `
		UPDATE agent_instances
		SET instance_name=$2, status=$3, workspace=$4, endpoint=$5, port=$6, error=$7, updated_at=$8
		WHERE user_id=$1`,
		inst.UserID, inst.InstanceName, string(inst.Status), inst.Workspace, inst.Endpoint, inst.Port, inst.Error, inst.UpdatedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) DeleteInstance(ctx context.Context, userID string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM agent_instances WHERE user_id=$1`, userID)
	if err != nil {
		return err
	}
	_, _ = p.pool.Exec(ctx, `DELETE FROM agent_activity WHERE user_id=$1`, userID)
	return nil
}

func (p *Postgres) TouchActivity(ctx context.Context, userID string) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO agent_activity (user_id, last_active_at, ws_connections)
		VALUES ($1, now(), 0)
		ON CONFLICT (user_id) DO UPDATE SET last_active_at = now()`, userID)
	return err
}

func (p *Postgres) SetWSConnections(ctx context.Context, userID string, n int) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO agent_activity (user_id, last_active_at, ws_connections)
		VALUES ($1, now(), $2)
		ON CONFLICT (user_id) DO UPDATE SET last_active_at = now(), ws_connections = $2`,
		userID, n)
	return err
}

func (p *Postgres) GetActivity(ctx context.Context, userID string) (*Activity, error) {
	var act Activity
	err := p.pool.QueryRow(ctx, `
		SELECT user_id, last_active_at, ws_connections FROM agent_activity WHERE user_id=$1`, userID).
		Scan(&act.UserID, &act.LastActiveAt, &act.WSConnections)
	if err == pgx.ErrNoRows {
		return &Activity{UserID: userID, LastActiveAt: time.Now()}, nil
	}
	return &act, err
}

func (p *Postgres) CreateReview(ctx context.Context, r *models.Review) error {
	findings, _ := json.Marshal(r.Findings)
	_, err := p.pool.Exec(ctx, `
		INSERT INTO code_reviews (review_id, user_id, repo, pr_number, status, model, findings, error, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		r.ID, r.UserID, r.Repo, r.PRNumber, r.Status, r.Model, findings, r.Error, r.CreatedAt, r.UpdatedAt)
	return err
}

func (p *Postgres) GetReview(ctx context.Context, userID, reviewID string) (*models.Review, error) {
	r, err := p.scanReview(p.pool.QueryRow(ctx, `
		SELECT review_id, user_id, repo, pr_number, status, model, findings, error, created_at, updated_at
		FROM code_reviews WHERE review_id=$1 AND user_id=$2`, reviewID, userID))
	if err == pgx.ErrNoRows {
		return nil, ErrNotFound
	}
	return r, err
}

func (p *Postgres) ListReviews(ctx context.Context, userID string) ([]*models.Review, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT review_id, user_id, repo, pr_number, status, model, findings, error, created_at, updated_at
		FROM code_reviews WHERE user_id=$1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*models.Review, 0)
	for rows.Next() {
		r, err := p.scanReview(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (p *Postgres) UpdateReview(ctx context.Context, r *models.Review) error {
	findings, _ := json.Marshal(r.Findings)
	tag, err := p.pool.Exec(ctx, `
		UPDATE code_reviews SET status=$3, findings=$4, error=$5, updated_at=$6 WHERE review_id=$1 AND user_id=$2`,
		r.ID, r.UserID, r.Status, findings, r.Error, r.UpdatedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) SaveVCSToken(ctx context.Context, t *models.VCSToken) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO user_vcs_tokens (user_id, platform, token) VALUES ($1,$2,$3)
		ON CONFLICT (user_id, platform) DO UPDATE SET token = $3`,
		t.UserID, t.Platform, t.Token)
	return err
}

func (p *Postgres) GetVCSToken(ctx context.Context, userID, platform string) (*models.VCSToken, error) {
	var t models.VCSToken
	err := p.pool.QueryRow(ctx, `
		SELECT user_id, platform, token FROM user_vcs_tokens WHERE user_id=$1 AND platform=$2`,
		userID, platform).Scan(&t.UserID, &t.Platform, &t.Token)
	if err == pgx.ErrNoRows {
		return nil, ErrNotFound
	}
	return &t, err
}

func (p *Postgres) Close() error {
	p.pool.Close()
	return nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanInstance(row rowScanner) (*models.Instance, error) {
	var inst models.Instance
	var status string
	err := row.Scan(&inst.UserID, &inst.InstanceName, &status, &inst.Workspace, &inst.Endpoint,
		&inst.Port, &inst.Error, &inst.CreatedAt, &inst.UpdatedAt)
	inst.Status = models.InstanceStatus(status)
	return &inst, err
}

func (p *Postgres) scanReview(row rowScanner) (*models.Review, error) {
	var r models.Review
	var findings []byte
	err := row.Scan(&r.ID, &r.UserID, &r.Repo, &r.PRNumber, &r.Status, &r.Model,
		&findings, &r.Error, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if len(findings) > 0 {
		_ = json.Unmarshal(findings, &r.Findings)
	}
	return &r, nil
}
