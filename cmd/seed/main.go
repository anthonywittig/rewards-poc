// Command seed fills a running stack with demo customers, driving the HTTP
// API rather than the Temporal client so seeding exercises the same path a
// user takes.
//
// Idempotent: customers that already exist are left alone. Points are reached
// by repeated adds, because that is the only way points enter the system.
// `make destroy && make up` is the clean slate.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
)

// customer is one seeded record.
type customer struct {
	id, name   string
	adds       []int
	deactivate bool
	why        string
}

// One customer per interesting state; six in total so the unfiltered list
// also demonstrates the "Showing 5 of 6" cap.
var seedSet = []customer{
	{
		id: "newbie", name: "Newly Enrolled",
		why: "enrolled, never earned -- the empty-timeline case",
	},
	{
		id: "katherine", name: "Katherine Johnson", adds: []int{95},
		why: "basic tier",
	},
	{
		id: "ada", name: "Ada Lovelace",
		adds: []int{120, 200, 180, 60, 40, 20, 20},
		why:  "gold, a few runs in -- shows continue-as-new",
	},
	{
		id: "grace", name: "Grace Hopper",
		adds: []int{500, 500, 250, 250},
		why:  "platinum: top tier, nextTierAt is 0",
	},
	{
		id: "departed", name: "Gone Away",
		adds: []int{100, 100, 100, 10}, deactivate: true,
		why: "deactivated: workflow completed, balance frozen for good",
	},
	{
		id: "capped", name: "Max Capacity",
		adds: []int{1000, 1000, 1000, 1000, 960},
		why:  "at 4960 of the 5000 cap, so any add over 40 is a handler rejection",
	},
}

func main() {
	// The compose one-shot sets this to the in-network address; the default is
	// the published port for running directly with `go run`.
	base := env("API_BASE", "http://localhost:8081")

	if err := ping(base); err != nil {
		log.Fatalf("no API at %s: %v\nis the stack up? try `make ps`, then `make up`", base, err)
	}

	created, existing, failed := 0, 0, 0
	for _, c := range seedSet {
		switch madeNew, err := ensure(base, c); {
		case err != nil:
			failed++
			log.Printf("  %-10s FAILED: %v", c.id, err)
		case madeNew:
			created++
			fmt.Printf("  %-10s created   %s\n", c.id, c.why)
		default:
			existing++
			fmt.Printf("  %-10s already exists\n", c.id)
		}
	}

	fmt.Printf("\n%d created, %d already existed, %d failed\n", created, existing, failed)
	if failed > 0 {
		os.Exit(1)
	}

	fmt.Printf("\n  %s/api/customers\n", env("API_PUBLIC_BASE", base))
	fmt.Printf("  %s/api/customers/ada/audit\n", env("API_PUBLIC_BASE", base))
}

// ensure creates the customer if absent. Reports whether it created one.
func ensure(base string, c customer) (bool, error) {
	if err := exists(base, c.id); err == nil {
		return false, nil
	} else if !isNotFound(err) {
		return false, err
	}

	if err := create(base, c); err != nil {
		return false, err
	}
	return true, nil
}

func exists(base, id string) error {
	return do(http.MethodGet, base+"/api/customers/"+id, nil, nil)
}

// isNotFound reports whether the API said the customer does not exist, as
// opposed to being unable to answer.
func isNotFound(err error) bool {
	var he *httpError
	return errors.As(err, &he) && he.status == http.StatusNotFound
}

func create(base string, c customer) error {
	err := do(http.MethodPost, base+"/api/customers", map[string]string{
		"customerId": c.id, "name": c.name,
	}, nil)
	if err != nil {
		return err
	}

	for i, amount := range c.adds {
		body := map[string]any{
			"amount": amount,
			"reason": fmt.Sprintf("seed purchase %d", i+1),
			// Idempotency key, deterministic so a re-run is traceable.
			"requestId": fmt.Sprintf("seed-%s-%03d", c.id, i+1),
		}
		if err := do(http.MethodPost, base+"/api/customers/"+c.id+"/points", body, nil); err != nil {
			return fmt.Errorf("add %d: %w", i+1, err)
		}
	}

	if c.deactivate {
		return do(http.MethodDelete, base+"/api/customers/"+c.id, nil, nil)
	}
	return nil
}

func ping(base string) error {
	resp, err := http.Get(base + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz returned %s", resp.Status)
	}
	return nil
}

// do sends a request and turns a non-2xx into the API's own error message.
func do(method, url string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		var e struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		raw, _ := io.ReadAll(resp.Body)
		if json.Unmarshal(raw, &e) == nil && e.Error.Code != "" {
			return &httpError{resp.StatusCode, e.Error.Message}
		}
		return &httpError{resp.StatusCode, string(bytes.TrimSpace(raw))}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// httpError carries the status alongside the API's own words.
type httpError struct {
	status  int
	message string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.status, e.message)
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
