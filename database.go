package gitbase

import (
	"gopkg.in/src-d/go-mysql-server.v0/sql"
)

const (
	// ReferencesTableName is the name of the refs table.
	ReferencesTableName = "refs"
	// CommitsTableName is the name of the commits table.
	CommitsTableName = "commits"
	// BlobsTableName is the name of the blobs table.
	BlobsTableName = "blobs"
	// TreeEntriesTableName is the name of the tree entries table.
	TreeEntriesTableName = "tree_entries"
	// RepositoriesTableName is the name of the repositories table.
	RepositoriesTableName = "repositories"
	// RemotesTableName is the name of the remotes table.
	RemotesTableName = "remotes"
)

// Database holds all git repository tables
type Database struct {
	name         string
	commits      sql.Table
	references   sql.Table
	treeEntries  sql.Table
	blobs        sql.Table
	repositories sql.Table
	remotes      sql.Table
}

// NewDatabase creates a new Database structure and initializes its
// tables with the given pool
func NewDatabase(name string) sql.Database {
	return &Database{
		name:         name,
		commits:      newCommitsTable(),
		references:   newReferencesTable(),
		blobs:        newBlobsTable(),
		treeEntries:  newTreeEntriesTable(),
		repositories: newRepositoriesTable(),
		remotes:      newRemotesTable(),
	}
}

// Name returns the name of the database
func (d *Database) Name() string {
	return d.name
}

// Tables returns a map with all initialized tables
func (d *Database) Tables() map[string]sql.Table {
	return map[string]sql.Table{
		CommitsTableName:      d.commits,
		ReferencesTableName:   d.references,
		BlobsTableName:        d.blobs,
		TreeEntriesTableName:  d.treeEntries,
		RepositoriesTableName: d.repositories,
		RemotesTableName:      d.remotes,
	}
}
