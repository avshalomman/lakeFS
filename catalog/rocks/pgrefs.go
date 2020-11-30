package rocks

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"

	"github.com/jackc/pgtype"
	"github.com/treeverse/lakefs/db"
)

type PgBranch struct {
	CommitID     CommitID     `db:"commit_id"`
	StagingToken StagingToken `db:"staging_token"`
}

func (ps CommitParents) Value() (driver.Value, error) {
	if ps == nil {
		return []string{}, nil
	}
	vs := make([]string, len(ps))
	for i, v := range ps {
		vs[i] = string(v)
	}
	return vs, nil
}

func (ps *CommitParents) Scan(src interface{}) error {
	p := pgtype.TextArray{}
	err := p.Scan(src)
	if err != nil {
		return err
	}
	for _, v := range p.Elements {
		*ps = append(*ps, CommitID(v.String))
	}
	return nil
}

type PGRefManager struct {
	db db.Database
}

func NewPGRefManager(db db.Database) *PGRefManager {
	return &PGRefManager{db}
}

func (m *PGRefManager) GetRepository(ctx context.Context, repositoryID RepositoryID) (*Repository, error) {
	repository, err := m.db.Transact(func(tx db.Tx) (interface{}, error) {
		repository := &Repository{}
		err := tx.Get(repository,
			`SELECT storage_namespace, creation_date, default_branch FROM kv_repositories WHERE id = $1`,
			repositoryID)
		if err != nil {
			return nil, err
		}
		return repository, nil
	}, db.ReadOnly(), db.WithContext(ctx))
	if errors.Is(err, db.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return repository.(*Repository), nil
}

func (m *PGRefManager) CreateRepository(ctx context.Context, repositoryID RepositoryID, repository Repository, branch Branch) error {
	_, err := m.db.Transact(func(tx db.Tx) (interface{}, error) {
		_, err := tx.Exec(
			`INSERT INTO kv_repositories (id, storage_namespace, creation_date, default_branch) VALUES ($1, $2, $3, $4)`,
			repositoryID, repository.StorageNamespace, repository.CreationDate, repository.DefaultBranchID)
		if err != nil {
			return nil, err
		}
		_, err = tx.Exec(`
				INSERT INTO kv_branches (repository_id, id, staging_token, commit_id)
				VALUES ($1, $2, $3, $4)`,
			repositoryID, repository.DefaultBranchID, branch.stagingToken, branch.CommitID)
		return nil, err
	}, db.WithContext(ctx))
	return err
}

func (m *PGRefManager) ListRepositories(ctx context.Context, from RepositoryID) (RepositoryIterator, error) {
	return NewRepositoryIterator(ctx, m.db, IteratorPrefetchSize, string(from)), nil
}

func (m *PGRefManager) DeleteRepository(ctx context.Context, repositoryID RepositoryID) error {
	_, err := m.db.Transact(func(tx db.Tx) (interface{}, error) {
		var err error
		_, err = tx.Exec(`DELETE FROM kv_branches WHERE repository_id = $1`, repositoryID)
		if err != nil {
			return nil, err
		}
		_, err = tx.Exec(`DELETE FROM kv_commits WHERE repository_id = $1`, repositoryID)
		if err != nil {
			return nil, err
		}
		_, err = tx.Exec(`DELETE FROM kv_repositories WHERE id = $1`, repositoryID)
		return nil, err
	}, db.WithContext(ctx))
	return err
}

func (m *PGRefManager) RevParse(ctx context.Context, repositoryID RepositoryID, ref Ref) (Reference, error) {
	return ResolveRef(ctx, m, repositoryID, ref)
}

func (m *PGRefManager) GetBranch(ctx context.Context, repositoryID RepositoryID, branchID BranchID) (*Branch, error) {
	branch, err := m.db.Transact(func(tx db.Tx) (interface{}, error) {
		pbranch := &PgBranch{}
		err := tx.Get(pbranch,
			`SELECT staging_token, commit_id FROM kv_branches WHERE repository_id = $1 AND id = $2`,
			repositoryID, branchID)
		if err != nil {
			return nil, err
		}
		return &Branch{
			CommitID:     pbranch.CommitID,
			stagingToken: pbranch.StagingToken,
		}, nil
	}, db.ReadOnly(), db.WithContext(ctx))
	if errors.Is(err, db.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return branch.(*Branch), nil
}

func (m *PGRefManager) SetBranch(ctx context.Context, repositoryID RepositoryID, branchID BranchID, branch Branch) error {
	_, err := m.db.Transact(func(tx db.Tx) (interface{}, error) {
		_, err := tx.Exec(`
			INSERT INTO kv_branches (repository_id, id, staging_token, commit_id)
			VALUES ($1, $2, $3, $4)
				ON CONFLICT (repository_id, id)
				DO UPDATE SET staging_token = $3, commit_id = $4`,
			repositoryID, branchID, branch.stagingToken, branch.CommitID)
		return nil, err
	}, db.WithContext(ctx))
	return err
}

func (m *PGRefManager) DeleteBranch(ctx context.Context, repositoryID RepositoryID, branchID BranchID) error {
	_, err := m.db.Transact(func(tx db.Tx) (interface{}, error) {
		_, err := tx.Exec(
			`DELETE FROM kv_branches WHERE repository_id = $1 AND id = $2`,
			repositoryID, branchID)
		return nil, err
	}, db.WithContext(ctx))
	return err
}

func (m *PGRefManager) ListBranches(ctx context.Context, repositoryID RepositoryID, from BranchID) (BranchIterator, error) {
	return NewBranchIterator(ctx, m.db, repositoryID, IteratorPrefetchSize, string(from)), nil
}

func (m *PGRefManager) GetCommit(ctx context.Context, repositoryID RepositoryID, commitID CommitID) (*Commit, error) {
	commit, err := m.db.Transact(func(tx db.Tx) (interface{}, error) {
		records := make([]*CommitRecord, 0)
		err := tx.Select(&records, `
					SELECT id, committer, message, creation_date, parents, tree_id, metadata
					FROM kv_commits
					WHERE repository_id = $1 AND id >= $2
					LIMIT 2`,
			repositoryID, commitID)
		if errors.Is(err, db.ErrNotFound) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		startWith := make([]*Commit, 0)
		for _, c := range records {
			if strings.HasPrefix(string(c.CommitID), string(commitID)) {
				startWith = append(startWith, c.Commit)
			}
		}
		if len(startWith) == 0 || len(startWith) > 1 {
			return "", ErrNotFound // empty or ambiguous
		}
		return startWith[0], nil
	}, db.ReadOnly(), db.WithContext(ctx))
	if errors.Is(err, db.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return commit.(*Commit), nil
}

func (m *PGRefManager) AddCommit(ctx context.Context, repositoryID RepositoryID, commit Commit) (CommitID, error) {
	_, err := m.db.Transact(func(tx db.Tx) (interface{}, error) {
		// commits are written based on their content hash, if we insert the same ID again,
		// it will necessarily have the same attributes as the existing one, so no need to overwrite it
		_, err := tx.Exec(`
				INSERT INTO kv_commits 
				(repository_id, id, committer, message, creation_date, parents, tree_id, metadata)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
				ON CONFLICT DO NOTHING`,
			repositoryID, commit.ID(), commit.Committer, commit.Message,
			commit.CreationDate, commit.Parents, commit.TreeID, commit.Metadata)
		return nil, err
	}, db.WithContext(ctx))
	if err != nil {
		return "", err
	}
	return commit.ID(), err
}

func (m *PGRefManager) FindMergeBase(ctx context.Context, repositoryID RepositoryID, commitIDs ...CommitID) (*Commit, error) {
	// TODO(ozkatz): This actually has some logic to it!
	panic("implement me")
}

func (m *PGRefManager) Log(ctx context.Context, repositoryID RepositoryID, from CommitID) (CommitIterator, error) {
	return NewCommitIterator(ctx, m.db, repositoryID, from), nil
}
