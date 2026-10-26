// Command simulator seeds a QA backend with 50 branches and drives it with
// automatic ticket, appointment and counter traffic.
//
//	simulator seed -admin-password '...'
//	simulator run  -admin-password '...' -duration 2m -ticket-rate 30
//	simulator verify -admin-password '...'
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"
)

const usage = `usage: simulator <command> [flags]

commands:
  seed   create 50 branches (services, counters), customer accounts and a display account
  run    send tickets, appointments and counter calls to the seeded branches
  verify check every requirement (F-1…F-6, T-1…T-4) against the API and print a PASS/FAIL table

The admin account is the first seed user, ` + FirstSeedUser + `, which the backend
creates at startup when ADMIN_PASSWORD is set. Pass the same password with
-admin-password or SIM_ADMIN_PASSWORD.

Run "simulator <command> -h" for the flags of each command.
`

func main() {
	log.SetFlags(log.Ltime)
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	cmd, args := os.Args[1], os.Args[2:]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	apiURL := fs.String("api", envOr("SIM_API_URL", "http://localhost:8080"), "backend base URL")
	adminEmail := fs.String("admin-email", FirstSeedUser, "admin account (the first seed user)")
	adminPassword := fs.String("admin-password", os.Getenv("SIM_ADMIN_PASSWORD"), "admin password (or SIM_ADMIN_PASSWORD)")
	branches := fs.Int("branches", 50, "number of branches")
	customers := fs.Int("customers", 200, "number of simulated customer accounts")
	customerPassword := fs.String("customer-password", envOr("SIM_CUSTOMER_PASSWORD", "SimCustomer-2026!"), "password for simulated customers")
	workers := fs.Int("workers", 32, "concurrent HTTP workers")

	var (
		counters        *int
		displayPassword *string
		duration        *time.Duration
		ticketRate      *float64
		apptRate        *float64
		callRate        *float64
		retryRatio      *float64
		seed            *int64
	)
	switch cmd {
	case "seed":
		counters = fs.Int("counters", 4, "counters per branch")
		displayPassword = fs.String("display-password", envOr("SIM_DISPLAY_PASSWORD", "SimDisplay-2026!"), "password for "+DisplayUser)
	case "run":
		duration = fs.Duration("duration", time.Minute, "how long to generate load")
		ticketRate = fs.Float64("ticket-rate", 20, "walk-in tickets per second across all branches")
		apptRate = fs.Float64("appointment-rate", 5, "appointments per second across all branches")
		callRate = fs.Float64("call-rate", 15, "counter call-next operations per second across all branches")
		retryRatio = fs.Float64("retry-ratio", 0.1, "share of creates re-sent with the same Idempotency-Key")
		seed = fs.Int64("seed", time.Now().UnixNano(), "random seed for reproducible traffic")
	case "verify":
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	_ = fs.Parse(args)

	if *adminPassword == "" {
		log.Fatal("admin password required: -admin-password or SIM_ADMIN_PASSWORD")
	}
	api := NewAPI(*apiURL, *workers)
	admin := NewSession(api, *adminEmail, *adminPassword)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var err error
	switch cmd {
	case "seed":
		err = Seed(api, admin, SeedConfig{
			Branches:         *branches,
			CountersPerBr:    *counters,
			Customers:        *customers,
			CustomerPassword: *customerPassword,
			DisplayPassword:  *displayPassword,
			Workers:          min(*workers, 8), // registration is Argon2-bound on the server
		})
	case "run":
		err = Run(ctx, api, admin, RunConfig{
			Duration:         *duration,
			TicketRate:       *ticketRate,
			AppointmentRate:  *apptRate,
			CallRate:         *callRate,
			RetryRatio:       *retryRatio,
			Workers:          *workers,
			Branches:         *branches,
			Customers:        *customers,
			CustomerPassword: *customerPassword,
			Seed:             *seed,
		})
	case "verify":
		err = Verify(api, admin, *apiURL)
	}
	if err != nil {
		log.Fatalf("%s: %v", cmd, err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
