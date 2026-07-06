// Package memory provides an in-memory store.Store for tests.
package memory

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/bykclk/gocov/internal/store"
)

// Store is an in-memory implementation of store.Store. Safe for concurrent use.
type Store struct {
	mu      sync.Mutex
	repoSeq int64
	upSeq   int64
	repos   map[int64]*store.Repo
	uploads map[int64]*store.Upload
	files   map[int64][]*store.UploadFile // keyed by upload ID
}

// New returns an empty in-memory store.
func New() *Store {
	return &Store{
		repos:   map[int64]*store.Repo{},
		uploads: map[int64]*store.Upload{},
		files:   map[int64][]*store.UploadFile{},
	}
}

func (s *Store) CreateRepo(_ context.Context, r *store.Repo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repoSeq++
	r.ID = s.repoSeq
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now()
	}
	cp := *r
	s.repos[r.ID] = &cp
	return nil
}

func (s *Store) RepoByID(_ context.Context, id int64) (*store.Repo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.repos[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *r
	return &cp, nil
}

func (s *Store) RepoBySlug(_ context.Context, slug string) (*store.Repo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.repos {
		if r.Slug == slug {
			cp := *r
			return &cp, nil
		}
	}
	return nil, store.ErrNotFound
}

func (s *Store) RepoByToken(_ context.Context, token string) (*store.Repo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.repos {
		if r.Token == token {
			cp := *r
			return &cp, nil
		}
	}
	return nil, store.ErrNotFound
}

func (s *Store) ListRepos(_ context.Context) ([]*store.Repo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*store.Repo, 0, len(s.repos))
	for _, r := range s.repos {
		cp := *r
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}

func (s *Store) CreateUpload(_ context.Context, u *store.Upload, files []*store.UploadFile) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upSeq++
	u.ID = s.upSeq
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now()
	}
	cp := *u
	s.uploads[u.ID] = &cp
	fs := make([]*store.UploadFile, 0, len(files))
	for _, f := range files {
		fcp := *f
		fcp.UploadID = u.ID
		fs = append(fs, &fcp)
	}
	sort.Slice(fs, func(i, j int) bool { return fs[i].Path < fs[j].Path })
	s.files[u.ID] = fs
	return nil
}

func (s *Store) Upload(_ context.Context, id int64) (*store.Upload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.uploads[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *u
	return &cp, nil
}

func (s *Store) ListUploads(_ context.Context, repoID int64, limit int) ([]*store.Upload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*store.Upload
	for _, u := range s.uploads {
		if u.RepoID == repoID {
			cp := *u
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *Store) UploadFiles(_ context.Context, uploadID int64) ([]*store.UploadFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fs, ok := s.files[uploadID]
	if !ok {
		return nil, store.ErrNotFound
	}
	out := make([]*store.UploadFile, 0, len(fs))
	for _, f := range fs {
		cp := *f
		out = append(out, &cp)
	}
	return out, nil
}

func (s *Store) LatestUpload(_ context.Context, repoID int64, branch string) (*store.Upload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var latest *store.Upload
	for _, u := range s.uploads {
		if u.RepoID == repoID && u.Branch == branch {
			if latest == nil || u.ID > latest.ID {
				latest = u
			}
		}
	}
	if latest == nil {
		return nil, store.ErrNotFound
	}
	cp := *latest
	return &cp, nil
}
