// Command hook points the App's webhook at a URL, with the secret the
// server checks deliveries against:
//
//	set -a; . ~/.config/deps-merger-cloud/dev.env; set +a
//	go run ./cmd/hook https://deps-merger.example.com/webhook
//	go run ./cmd/hook deliveries   # the latest deliveries and their status
//
// It reads DMC_APP_ID, DMC_PRIVATE_KEY or DMC_PRIVATE_KEY_FILE, and
// DMC_WEBHOOK_SECRET. Turning the webhook on and choosing its events stay
// in the App's settings: GitHub has no API for them.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"

	"github.com/sglavoie/deps-merger/internal/app"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "hook:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 2 {
		return errors.New("usage: hook https://host/webhook | deliveries")
	}
	id, err := strconv.ParseInt(os.Getenv("DMC_APP_ID"), 10, 64)
	if err != nil {
		return fmt.Errorf("DMC_APP_ID: %w", err)
	}
	key := []byte(os.Getenv("DMC_PRIVATE_KEY"))
	if len(key) == 0 {
		if key, err = os.ReadFile(os.Getenv("DMC_PRIVATE_KEY_FILE")); err != nil {
			return fmt.Errorf("private key: %w", err)
		}
	}
	ctx := context.Background()
	a, err := app.New(ctx, id, key)
	if err != nil {
		return err
	}
	if os.Args[1] == "deliveries" {
		fmt.Printf("%s subscribes to: %v\n", a.Slug, a.Events)
		ds, err := a.Deliveries(ctx, 20)
		if err != nil {
			return err
		}
		for _, d := range ds {
			event := d.Event
			if d.Action != "" {
				event += "." + d.Action
			}
			redelivery := ""
			if d.Redelivery {
				redelivery = " (redelivery)"
			}
			fmt.Printf("%s  %-36s %d %s%s\n", d.At.UTC().Format("2006-01-02 15:04:05"), event, d.StatusCode, d.Status, redelivery)
		}
		return nil
	}
	u, err := url.Parse(os.Args[1])
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("%q is not an https URL", os.Args[1])
	}
	secret := os.Getenv("DMC_WEBHOOK_SECRET")
	if secret == "" {
		return errors.New("DMC_WEBHOOK_SECRET is not set")
	}
	if err := a.SetWebhook(ctx, u.String(), secret); err != nil {
		return err
	}
	fmt.Printf("%s now delivers to %s\n", a.Slug, u)
	return nil
}
