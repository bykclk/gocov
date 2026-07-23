package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"
)

type uploadRequest struct {
	Server      string
	Token       string
	Format      string
	PathPrefix  string
	ProfilePath string
	Build       buildInfo
}

// uploadResponse mirrors the server's POST /api/v1/upload response.
type uploadResponse struct {
	ID           int64    `json:"id"`
	TotalPct     float64  `json:"total_pct"`
	CoveredStmts int64    `json:"covered_stmts"`
	TotalStmts   int64    `json:"total_stmts"`
	DeltaPct     *float64 `json:"delta_pct"`
	BuildStatus  string   `json:"build_status"`
	RepoCreated  bool     `json:"repo_created"`

	DiffPct          *float64 `json:"diff_pct"`
	DiffCoveredLines *int64   `json:"diff_covered_lines"`
	DiffTotalLines   *int64   `json:"diff_total_lines"`
	DiffStatus       string   `json:"diff_status"`
	PRComment        string   `json:"pr_comment"`
}

func upload(req uploadRequest) (*uploadResponse, error) {
	profileData, err := os.ReadFile(req.ProfilePath)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fields := map[string]string{
		"repo":        req.Build.Repo,
		"commit":      req.Build.Commit,
		"branch":      req.Build.Branch,
		"pr_id":       req.Build.PRID,
		"format":      req.Format,
		"path_prefix": req.PathPrefix,
	}
	for k, v := range fields {
		if v == "" {
			continue
		}
		if err := mw.WriteField(k, v); err != nil {
			return nil, err
		}
	}
	fw, err := mw.CreateFormFile("profile", "coverage.out")
	if err != nil {
		return nil, err
	}
	if _, err := fw.Write(profileData); err != nil {
		return nil, err
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}

	url := strings.TrimSuffix(req.Server, "/") + "/api/v1/upload"
	httpReq, err := http.NewRequest(http.MethodPost, url, &buf)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+req.Token)
	httpReq.Header.Set("Content-Type", mw.FormDataContentType())

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusCreated {
		var apiErr struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &apiErr) == nil && apiErr.Error != "" {
			return nil, fmt.Errorf("server returned %d: %s", resp.StatusCode, apiErr.Error)
		}
		return nil, fmt.Errorf("server returned %d: %s", resp.StatusCode, body)
	}
	var out uploadResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &out, nil
}
