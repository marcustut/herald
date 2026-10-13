package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/marcustut/herald/internal/adapters/hash"
	"github.com/marcustut/herald/internal/adapters/postgres"
	"github.com/marcustut/herald/internal/adapters/secret"
	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/domain"
)

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func runToken(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: herald token <create|revoke> ...")
		return 2
	}
	switch args[0] {
	case "create":
		return runTokenCreate(ctx, args[1:], stdout, stderr)
	case "revoke":
		return runTokenRevoke(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown token subcommand %q\n", args[0])
		return 2
	}
}

func runTokenCreate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("token create", stderr)
	dsn := fs.String("database-url", os.Getenv("HERALD_DATABASE_URL"), "PostgreSQL connection string")
	tenant := fs.String("tenant", firstNonEmpty(os.Getenv("HERALD_TENANT"), "owner"), "tenant id")
	project := fs.String("project", "", "project slug (project token)")
	scoped := fs.String("scoped", "", "comma-separated projects (scoped token)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dsn == "" {
		fmt.Fprintln(stderr, "error: --database-url or HERALD_DATABASE_URL is required")
		return 1
	}
	if *project == "" && *scoped == "" {
		fmt.Fprintln(stderr, "error: --project or --scoped is required")
		return 1
	}

	creds, closeStore, err := openCredentials(ctx, *dsn)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	defer closeStore()

	issuer := app.Issuer{Store: creds, Hasher: hash.SHA256{}, Secrets: secret.Generator{}, Clock: systemClock{}}
	var token string
	if *scoped != "" {
		projects := make([]domain.ProjectID, 0)
		for _, p := range strings.Split(*scoped, ",") {
			if p = strings.TrimSpace(p); p != "" {
				projects = append(projects, domain.ProjectID(p))
			}
		}
		token, err = issuer.IssueScopedToken(ctx, domain.TenantID(*tenant), projects)
	} else {
		token, err = issuer.IssueProjectToken(ctx, domain.TenantID(*tenant), domain.ProjectID(*project))
	}
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintln(stdout, token)
	return 0
}

func runTokenRevoke(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("token revoke", stderr)
	dsn := fs.String("database-url", os.Getenv("HERALD_DATABASE_URL"), "PostgreSQL connection string")
	tenant := fs.String("tenant", firstNonEmpty(os.Getenv("HERALD_TENANT"), "owner"), "tenant id")
	project := fs.String("project", "", "project slug")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dsn == "" || *project == "" {
		fmt.Fprintln(stderr, "error: --database-url and --project are required")
		return 1
	}

	creds, closeStore, err := openCredentials(ctx, *dsn)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	defer closeStore()

	issuer := app.Issuer{Store: creds, Hasher: hash.SHA256{}, Secrets: secret.Generator{}, Clock: systemClock{}}
	if err := issuer.RevokeProject(ctx, domain.TenantID(*tenant), domain.ProjectID(*project)); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintln(stdout, "revoked", *project)
	return 0
}

func openCredentials(ctx context.Context, dsn string) (app.CredentialStore, func(), error) {
	pool, err := postgres.Open(ctx, dsn)
	if err != nil {
		return nil, nil, err
	}
	if err := postgres.Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, nil, err
	}
	return postgres.NewCredentials(pool), pool.Close, nil
}
