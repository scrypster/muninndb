package auth

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/cockroachdb/pebble"
	"golang.org/x/crypto/bcrypt"
)

// Store provides auth persistence on top of the shared Pebble database.
type Store struct {
	db            *pebble.DB
	DefaultPublic bool // when true, unconfigured vaults default to public (for dev/test)
}

func NewStore(db *pebble.DB) *Store {
	return &Store{db: db}
}

// CreateAdmin hashes the password and persists the admin user.
func (s *Store) CreateAdmin(username, password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	user := AdminUser{
		Username:  username,
		PassHash:  hash,
		CreatedAt: time.Now(),
	}
	data, err := json.Marshal(user)
	if err != nil {
		return fmt.Errorf("marshal admin: %w", err)
	}
	return s.db.Set(adminUserKey(username), data, pebble.Sync)
}

// ValidateAdmin returns nil if username/password are correct.
func (s *Store) ValidateAdmin(username, password string) error {
	data, closer, err := s.db.Get(adminUserKey(username))
	if err != nil {
		return fmt.Errorf("user not found")
	}
	defer closer.Close()

	var user AdminUser
	if err := json.Unmarshal(data, &user); err != nil {
		return fmt.Errorf("corrupt admin record: %w", err)
	}
	if err := bcrypt.CompareHashAndPassword(user.PassHash, []byte(password)); err != nil {
		return fmt.Errorf("invalid password")
	}
	return nil
}

// ChangeAdminPassword updates the password for an existing admin user.
func (s *Store) ChangeAdminPassword(username, newPassword string) error {
	key := adminUserKey(username)
	val, closer, err := s.db.Get(key)
	if err != nil {
		return fmt.Errorf("admin user not found: %w", err)
	}
	var u AdminUser
	if jsonErr := json.Unmarshal(val, &u); jsonErr != nil {
		closer.Close()
		return jsonErr
	}
	closer.Close()
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	u.PassHash = hash
	encoded, err := json.Marshal(u)
	if err != nil {
		return err
	}
	return s.db.Set(key, encoded, pebble.Sync)
}

// AdminExists returns true if at least one admin user has been created.
func (s *Store) AdminExists() bool {
	iter, err := s.db.NewIter(&pebble.IterOptions{
		LowerBound: []byte{prefixAdminUser},
		UpperBound: []byte{prefixAdminUser + 1},
	})
	if err != nil {
		return false
	}
	defer iter.Close()
	return iter.First()
}
