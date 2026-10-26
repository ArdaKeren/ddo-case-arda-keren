package auth

import (
	"context"
	"errors"
	"fmt"
	"log"
)

// BootstrapAdmin creates the first admin account when none exists yet.
// It is a no-op if email or password is empty or an admin is already present.
func BootstrapAdmin(ctx context.Context, store *Store, email, password string) error {
	if email == "" || password == "" {
		return nil
	}
	exists, err := store.HasAdmin(ctx)
	if err != nil || exists {
		return err
	}
	email, ok := NormalizeEmail(email)
	if !ok {
		return fmt.Errorf("ADMIN_EMAIL %q is not a valid address", email)
	}
	if msg := ValidatePassword(password); msg != "" {
		return fmt.Errorf("ADMIN_PASSWORD: %s", msg)
	}
	hash, err := HashPassword(password, DefaultArgon2)
	if err != nil {
		return err
	}
	if _, err := store.CreateUser(ctx, email, hash, "Administrator", RoleAdmin); err != nil {
		if errors.Is(err, ErrEmailTaken) {
			return fmt.Errorf("ADMIN_EMAIL %q already belongs to a non-admin user", email)
		}
		return err
	}
	log.Printf("bootstrap: created admin %s", email)
	return nil
}
