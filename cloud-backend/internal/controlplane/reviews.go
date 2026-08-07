package controlplane

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"cloude-agent/internal/models"
	"cloude-agent/internal/store"
)

// ReviewWorker 对应文档的 CodeReview Worker（异步评审）。
// 本地实现：扫描本地仓库中的 TODO/FIXME/HACK/XXX，输出行级 findings；
// GitHub/GitLab 的 PR 评审需在接入 VCS OAuth 后扩展（文档 user_vcs_tokens）。
type ReviewWorker struct {
	store     store.Store
	getConfig func(ctx context.Context, userID string) (*models.ModelConfig, error)
}

func NewReviewWorker(st store.Store, getConfig func(ctx context.Context, userID string) (*models.ModelConfig, error)) *ReviewWorker {
	return &ReviewWorker{store: st, getConfig: getConfig}
}

var reviewPattern = regexp.MustCompile(`(?i)\b(TODO|FIXME|HACK|XXX)\b[:：]?\s*(.*)`)

func (w *ReviewWorker) Submit(ctx context.Context, userID, repo string, prNumber int) (*models.Review, error) {
	cfg, err := w.getConfig(ctx, userID)
	if err != nil {
		cfg = &models.ModelConfig{Model: "unknown"}
	}
	now := time.Now().UTC()
	r := &models.Review{
		ID:        newReviewID(),
		UserID:    userID,
		Repo:      repo,
		PRNumber:  prNumber,
		Status:    "pending",
		Model:     cfg.Model,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := w.store.CreateReview(ctx, r); err != nil {
		return nil, err
	}
	go w.run(r.ID, userID, repo)
	return r, nil
}

func newReviewID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "rev_" + hex.EncodeToString(b)
}

func (w *ReviewWorker) run(reviewID, userID, repo string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	set := func(status string, findings []models.Finding, errMsg string) {
		r, err := w.store.GetReview(ctx, userID, reviewID)
		if err != nil {
			return
		}
		r.Status = status
		r.Findings = findings
		r.Error = errMsg
		r.UpdatedAt = time.Now().UTC()
		_ = w.store.UpdateReview(ctx, r)
	}

	set("running", nil, "")
	info, err := os.Stat(repo)
	if err != nil || !info.IsDir() {
		set("failed", nil, "本地评审仅支持目录路径；远程仓库评审需接入 VCS OAuth（user_vcs_tokens）")
		return
	}

	findings, err := scanRepo(repo)
	if err != nil {
		set("failed", nil, err.Error())
		return
	}
	set("completed", findings, "")
}

func scanRepo(root string) ([]models.Finding, error) {
	var out []models.Finding
	skip := map[string]bool{".git": true, "node_modules": true, "vendor": true, "bin": true, "data": true}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() && skip[info.Name()] {
			return filepath.SkipDir
		}
		if info.IsDir() || info.Size() > 1<<20 {
			return nil
		}
		if !isTextFile(path) {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 64*1024)
		lineNo := 0
		for scanner.Scan() {
			lineNo++
			line := scanner.Text()
			if m := reviewPattern.FindStringSubmatch(line); m != nil {
				rel, _ := filepath.Rel(root, path)
				out = append(out, models.Finding{
					File:    rel,
					Line:    lineNo,
					Level:   "warning",
					Message: fmt.Sprintf("%s: %s", strings.ToUpper(m[1]), strings.TrimSpace(m[2])),
				})
			}
		}
		return nil
	})
	return out, err
}

func isTextFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 512)
	n, _ := f.Read(head)
	if n == 0 {
		return false
	}
	for _, b := range head[:n] {
		if b == 0 {
			return false
		}
	}
	return true
}
