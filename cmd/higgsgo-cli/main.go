// higgsgo-cli — operations CLI.
//
// Subcommands (implemented incrementally):
//
//	import-accounts <path>   import Node-era higgsfield-*.json files into the DB
//	list-accounts            print pool summary
//	balance <account_id>     fetch and print live wallet snapshot
//	register <email>         run one registration attempt synchronously
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/greensheep999/higgsgo/internal/adapters/storage/sqlite"
	"github.com/greensheep999/higgsgo/internal/config"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "list-accounts":
		if err := cmdListAccounts(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "higgsgo-cli: %v\n", err)
			os.Exit(1)
		}
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "higgsgo-cli: unknown subcommand %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: higgsgo-cli <subcommand> [args]")
	fmt.Fprintln(os.Stderr, "  list-accounts [-config PATH]  list account pool contents")
}

func cmdListAccounts(args []string) error {
	configPath := "configs/higgsgo.example.toml"
	if len(args) >= 2 && args[0] == "-config" {
		configPath = args[1]
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if cfg.Storage.Driver != "sqlite" {
		return fmt.Errorf("only sqlite storage supported in CLI right now")
	}
	ctx := context.Background()
	db, err := sqlite.Open(ctx, cfg.Storage.SQLite.Path)
	if err != nil {
		return fmt.Errorf("open sqlite: %w", err)
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, `
		SELECT id, email, plan_type, status, subscription_balance, in_flight_jobs, has_unlim, has_flex_unlim
		FROM accounts
		ORDER BY plan_type, subscription_balance DESC`)
	if err != nil {
		return err
	}
	defer rows.Close()

	fmt.Printf("%-40s %-40s %-9s %-10s %10s %8s %5s %5s\n", "id", "email", "plan", "status", "sub_bal", "in_flt", "unlm", "flex")
	fmt.Println("--------------------------------------------------------------------------------------------------------------------------")
	for rows.Next() {
		var (
			id, email, plan, status string
			subBal                  int64
			inFlt                   int
			hasUnlim, hasFlex       int
		)
		if err := rows.Scan(&id, &email, &plan, &status, &subBal, &inFlt, &hasUnlim, &hasFlex); err != nil {
			return err
		}
		fmt.Printf("%-40s %-40s %-9s %-10s %10d %8d %5d %5d\n", id, email, plan, status, subBal, inFlt, hasUnlim, hasFlex)
	}
	return rows.Err()
}
