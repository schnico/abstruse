package core

import (
	"github.com/bleenco/abstruse/pkg/gitscm"
	"github.com/drone/go-scm/scm"
)

type (
	// Repository defines `repositories` db table.
	Repository struct {
		ID            uint     `gorm:"primary_key;auto_increment;not null" json:"id"`
		UID           string   `gorm:"not null" json:"uid"`
		ProviderName  string   `gorm:"not null" json:"providerName"`
		Namespace     string   `gorm:"not null" json:"namespace"`
		Name          string   `gorm:"not null;size:255" json:"name"`
		FullName      string   `gorm:"not null;size:255" json:"fullName"`
		Private       bool     `json:"private"`
		Fork          bool     `json:"fork"`
		URL           string   `json:"url"`
		Clone         string   `json:"clone"`
		CloneSSH      string   `json:"cloneSSH"`
		DefaultBranch string   `json:"defaultBranch"`
		Active        bool     `json:"active"`
		UserID        uint     `json:"userID"`
		User          User     `json:"-"`
		ProviderID    uint     `gorm:"not null" json:"providerID"`
		Provider      Provider `json:"-"`
		Timestamp
	}

	// RepositoryFilter defines filters when listing repositories
	// from the datastore.
	RepositoryFilter struct {
		Limit   int
		Offset  int
		Keyword string
		UserID  uint
	}

	// RepositoryStore defines operations on repositories in datastorage.
	RepositoryStore interface {
		// Find returns repository from the datastore.
		Find(uint) (Repository, error)

		// FindClone returns repository by clone URL from the datastore
		FindClone(string) (Repository, error)

		// List returns list of repositories from the datastore.
		List(RepositoryFilter) ([]Repository, int, error)

		// Create persists a new repository to the datastore.
		Create(Repository) error

		// Update persists updated repository to the datastore.
		Update(Repository) error

		// CreateOrUpdate persists new repository to the datastore
		// if not exists or updates exists one.
		CreateOrUpdate(Repository) error

		// Delete deletes repository from the datastore.
		Delete(Repository) error

		// SetActive persists new active status to the repository in the datastore.
		SetActive(uint, bool) error

		// ListHooks returns webhooks for specified repository.
		ListHooks(uint) ([]*scm.Hook, error)

		// CreateHook creates webhook for specified repository.
		CreateHook(uint, gitscm.HookForm) error

		// DeleteHooks deletes all related webhooks for specified repository
		DeleteHooks(uint) error
	}
)

// TableName is name that is used in db.
func (Repository) TableName() string {
	return "repositories"
}
