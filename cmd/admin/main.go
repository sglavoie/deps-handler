// Command admin works on the server's database, while it runs:
//
//	go run ./cmd/admin pause <installation> [reason]   # the kill switch
//	go run ./cmd/admin resume <installation>
//	go run ./cmd/admin status [days]                   # installations, writes
//	go run ./cmd/admin metrics [-json]                 # installation activity
//
// A pause stops an installation's merges from the next one on, a run in
// progress included; its issues are still kept up to date. It reads
// DMC_DB (default dmc.db).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sglavoie/deps-handler/internal/store"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "admin:", err)
		os.Exit(1)
	}
}

const usage = "usage: admin pause <installation> [reason] | resume <installation> | status [days] | metrics [-json]"

func run(args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	ctx := context.Background()
	db := os.Getenv("DMC_DB")
	if db == "" {
		db = "dmc.db"
	}
	if _, err := os.Stat(db); err != nil {
		return fmt.Errorf("database %s: %w", db, err)
	}
	st, err := store.Open(ctx, db)
	if err != nil {
		return err
	}
	defer st.Close()

	installation := func() (int64, error) {
		if len(args) < 2 {
			return 0, errors.New(usage)
		}
		return strconv.ParseInt(args[1], 10, 64)
	}
	switch args[0] {
	case "pause":
		id, err := installation()
		if err != nil {
			return err
		}
		if err := st.Pause(ctx, id, strings.Join(args[2:], " "), time.Now()); err != nil {
			return err
		}
		fmt.Printf("installation %d paused: no more merges until resumed\n", id)
	case "resume":
		id, err := installation()
		if err != nil {
			return err
		}
		if err := st.Resume(ctx, id); err != nil {
			return err
		}
		fmt.Printf("installation %d resumed\n", id)
	case "status":
		days := 7
		if len(args) > 1 {
			if days, err = strconv.Atoi(args[1]); err != nil || days < 1 {
				return errors.New(usage)
			}
		}
		return status(ctx, st, days)
	case "metrics":
		asJSON := len(args) > 1 && (args[1] == "-json" || args[1] == "--json")
		if len(args) > 1 && !asJSON {
			return errors.New(usage)
		}
		return metrics(ctx, st, asJSON)
	default:
		return errors.New(usage)
	}
	return nil
}

func status(ctx context.Context, st *store.Store, days int) error {
	ins, err := st.Installations(ctx)
	if err != nil {
		return err
	}
	fmt.Println("Installations:")
	for _, in := range ins {
		state := "active"
		if in.RemovedAt != nil {
			state = "removed " + in.RemovedAt.Format("2006-01-02")
		} else if p, err := st.Paused(ctx, in.ID); err != nil {
			return err
		} else if p {
			state = "PAUSED"
		}
		fmt.Printf("  %d  %-20s since %s  %s\n", in.ID, in.Account, in.AddedAt.Format("2006-01-02"), state)
	}
	since := time.Now().AddDate(0, 0, -days)
	ws, err := st.Writes(ctx, since)
	if err != nil {
		return err
	}
	fmt.Printf("Writes in the last %d days: %d\n", days, len(ws))
	for _, w := range ws {
		fmt.Printf("  %s  %-30s %-16s #%-5d %s\n", w.At.Local().Format("2006-01-02 15:04"), w.Repo, w.Kind, w.Number, w.Detail)
	}
	return nil
}

func metrics(ctx context.Context, st *store.Store, asJSON bool) error {
	ms, err := st.Metrics(ctx, time.Now())
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(ms)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "INSTALLATION\tDAYS\tREPOS\tMERGING\tPRS\tOPEN\tBY APP\tBY OTHERS\tCLOSED\tAPP SHARE\tAGE BEFORE\tAGE NOW\tTO MERGE (APP/OTHERS)\tREVIEWS (APPROVED)\tTOKENS IN/OUT\tAT DAY 28")
	for _, m := range ms {
		account := m.Account
		if m.RemovedAt != nil {
			account += " (removed)"
		}
		share := "-"
		if m.AppShare >= 0 {
			share = fmt.Sprintf("%.0f%%", 100*m.AppShare)
		}
		retained := "-"
		if m.Retained != nil {
			retained = map[bool]string{true: "stayed", false: "left"}[*m.Retained]
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%s\t%s\t%s\t%s / %s\t%d (%d)\t%d / %d\t%s\n", account, m.Days, m.Repos, m.ReposMerging,
			m.PRs, m.Open, m.MergedByApp, m.MergedOther, m.ClosedUnmerged, share,
			age(m.OpenAgeAtStart), age(m.OpenAgeNow), age(m.MergeTimeApp), age(m.MergeTimeOther),
			m.Reviews, m.ReviewsApproved, m.PromptTokens, m.CompletionTokens, retained)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	return nil
}

// age writes a duration in days, or hours under two days.
func age(d time.Duration) string {
	switch {
	case d == 0:
		return "-"
	case d < 48*time.Hour:
		return fmt.Sprintf("%.0fh", d.Hours())
	default:
		return fmt.Sprintf("%.0fd", d.Hours()/24)
	}
}
