package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
)

type Branch struct {
	ID   string `json:"id"`
	Code string `json:"code"`
	Name string `json:"name"`
}

type Service struct {
	ID       string `json:"id"`
	BranchID string `json:"branch_id"`
	Code     string `json:"code"`
}

type Counter struct {
	ID       string `json:"id"`
	BranchID string `json:"branch_id"`
	Name     string `json:"name"`
}

type SeedConfig struct {
	Branches         int
	CountersPerBr    int
	Customers        int
	CustomerPassword string
	DisplayPassword  string
	Workers          int
}

// Seed builds the simulated estate. It is idempotent: anything that already
// exists (409) is left as is, so it can be re-run safely.
func Seed(api *API, admin *Session, cfg SeedConfig) error {
	if err := requireAdmin(admin); err != nil {
		return err
	}
	if cfg.Branches > len(cities) {
		return fmt.Errorf("at most %d branches are supported", len(cities))
	}

	var existing struct {
		Branches []Branch `json:"branches"`
	}
	if err := admin.GetJSON("/api/queue/branches", &existing); err != nil {
		return fmt.Errorf("list branches: %w", err)
	}
	byCode := map[string]Branch{}
	for _, b := range existing.Branches {
		byCode[b.Code] = b
	}

	created := struct {
		sync.Mutex
		branches, services, counters, customers int
	}{}

	err := parallel(cfg.Workers, cfg.Branches, func(i int) error {
		code := branchCode(i + 1)
		b, ok := byCode[code]
		if !ok {
			res, err := admin.Do(http.MethodPost, "/api/queue/branches", mustJSON(map[string]string{
				"code": code, "name": cities[i] + " Şubesi",
			}), nil)
			if err != nil {
				return err
			}
			if res.Status != http.StatusCreated {
				return fmt.Errorf("create branch %s: %w", code, res.Err())
			}
			if err := res.Decode(&b); err != nil {
				return err
			}
			created.Lock()
			created.branches++
			created.Unlock()
		}
		s, c, err := seedBranch(admin, b, cfg.CountersPerBr)
		created.Lock()
		created.services += s
		created.counters += c
		created.Unlock()
		return err
	})
	if err != nil {
		return err
	}

	err = parallel(cfg.Workers, cfg.Customers, func(i int) error {
		made, err := register(api, customerEmail(i+1), cfg.CustomerPassword, fmt.Sprintf("Sim Müşteri %04d", i+1))
		if made {
			created.Lock()
			created.customers++
			created.Unlock()
		}
		return err
	})
	if err != nil {
		return err
	}

	if err := seedDisplayUser(api, admin, cfg.DisplayPassword); err != nil {
		return err
	}

	log.Printf("seed: created %d branches, %d services, %d counters, %d customers (existing ones kept)",
		created.branches, created.services, created.counters, created.customers)
	log.Printf("seed: admin=%s  display=%s  customers=%s … %s",
		FirstSeedUser, DisplayUser, customerEmail(1), customerEmail(cfg.Customers))
	return nil
}

func requireAdmin(admin *Session) error {
	var me struct {
		User struct {
			Role string `json:"role"`
		} `json:"user"`
	}
	if err := admin.GetJSON("/api/auth/me", &me); err != nil {
		if errors.Is(err, ErrLogin) {
			return fmt.Errorf("%w\n\nThe first seed user is created by the backend at startup. Start it with\n"+
				"  ADMIN_EMAIL=%s ADMIN_PASSWORD=<same as -admin-password>\non a database without an admin", err, FirstSeedUser)
		}
		return err
	}
	if me.User.Role != "admin" {
		return fmt.Errorf("%s has role %q; the simulator needs admin", admin.Email, me.User.Role)
	}
	return nil
}

func seedBranch(admin *Session, b Branch, counters int) (newServices, newCounters int, err error) {
	var svc struct {
		Services []Service `json:"services"`
	}
	if err := admin.GetJSON("/api/queue/services?branch_id="+b.ID, &svc); err != nil {
		return 0, 0, err
	}
	have := map[string]bool{}
	for _, s := range svc.Services {
		have[s.Code] = true
	}
	for _, s := range services {
		if have[s.Code] {
			continue
		}
		res, err := admin.Do(http.MethodPost, "/api/queue/services",
			mustJSON(map[string]string{"branch_id": b.ID, "code": s.Code, "name": s.Name}), nil)
		if err != nil {
			return newServices, newCounters, err
		}
		if res.Status != http.StatusCreated && res.Status != http.StatusConflict {
			return newServices, newCounters, fmt.Errorf("create service %s/%s: %w", b.Code, s.Code, res.Err())
		}
		if res.Status == http.StatusCreated {
			newServices++
		}
	}

	var cs struct {
		Counters []Counter `json:"counters"`
	}
	if err := admin.GetJSON("/api/queue/branches/"+b.ID+"/counters", &cs); err != nil {
		return newServices, newCounters, err
	}
	haveC := map[string]bool{}
	for _, c := range cs.Counters {
		haveC[c.Name] = true
	}
	for i := 1; i <= counters; i++ {
		name := fmt.Sprintf("Gişe %d", i)
		if haveC[name] {
			continue
		}
		res, err := admin.Do(http.MethodPost, "/api/queue/counters",
			mustJSON(map[string]string{"branch_id": b.ID, "name": name}), nil)
		if err != nil {
			return newServices, newCounters, err
		}
		if res.Status != http.StatusCreated && res.Status != http.StatusConflict {
			return newServices, newCounters, fmt.Errorf("create counter %s/%s: %w", b.Code, name, res.Err())
		}
		if res.Status == http.StatusCreated {
			newCounters++
		}
	}
	return newServices, newCounters, nil
}

// register returns created=false when the account already exists.
func register(api *API, email, password, name string) (created bool, err error) {
	res, err := api.Do(http.MethodPost, "/api/auth/register", "",
		mustJSON(map[string]string{"email": email, "password": password, "full_name": name}), nil)
	if err != nil {
		return false, err
	}
	switch res.Status {
	case http.StatusCreated:
		return true, nil
	case http.StatusConflict:
		return false, nil
	}
	return false, fmt.Errorf("register %s: %w", email, res.Err())
}

// seedDisplayUser creates the lobby-screen account and gives it the display role.
func seedDisplayUser(api *API, admin *Session, password string) error {
	if _, err := register(api, DisplayUser, password, "Lobi Ekranı"); err != nil {
		return err
	}
	id, err := findUserID(admin, DisplayUser)
	if err != nil {
		return err
	}
	res, err := admin.Do(http.MethodPatch, "/api/admin/users/"+id+"/role", mustJSON(map[string]string{"role": "display"}), nil)
	if err != nil {
		return err
	}
	if res.Status != http.StatusOK {
		return fmt.Errorf("set display role: %w", res.Err())
	}
	return nil
}

func findUserID(admin *Session, email string) (string, error) {
	for offset := 0; ; offset += 100 {
		var page struct {
			Users []struct {
				ID    string `json:"id"`
				Email string `json:"email"`
			} `json:"users"`
		}
		if err := admin.GetJSON(fmt.Sprintf("/api/admin/users?limit=100&offset=%d", offset), &page); err != nil {
			return "", err
		}
		for _, u := range page.Users {
			if u.Email == email {
				return u.ID, nil
			}
		}
		if len(page.Users) < 100 {
			return "", fmt.Errorf("user %s not found", email)
		}
	}
}

// parallel runs fn(0..n-1) on `workers` goroutines and returns the first error.
func parallel(workers, n int, fn func(i int) error) error {
	jobs := make(chan int)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				if err := fn(i); err != nil {
					errs <- err
				}
			}
		}()
	}
	for i := 0; i < n; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	close(errs)
	return <-errs // nil when the channel is empty
}
