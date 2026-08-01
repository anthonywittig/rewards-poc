// Command seed fills a running stack with a demo dataset, driving the HTTP API
// rather than the Temporal client.
//
//	make up       # runs this at the end, once the API answers
//	make seed     # to re-run it on its own
//	make reset    # the only true clean slate
//
// Read-then-create, never modify: deactivation is soft
// (FINDINGS.md#soft-deactivation), so re-enrolling an existing customer
// reactivates them with their points intact rather than resetting them.
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
	"time"
)

// customer is one seeded record. Points are reached by repeated adds, since
// that is the only way points ever enter the system.
type customer struct {
	id, name, email string
	adds            []int
	deactivate      bool
	why             string
}

// Six customers per tier (basic < 500, gold < 1000, platinum >= 1000), which
// fills the tier filter and pushes the unfiltered list past ListLimit.
var seedSet = []customer{
	// --- basic ----------------------------------------------------------------
	{
		id: "newbie", name: "Newly Enrolled", email: "new@example.com",
		why: "enrolled, never earned -- the empty-timeline case",
	},
	{
		id: "departed", name: "Gone Away", email: "gone@example.com",
		adds: []int{100, 100, 100, 10}, deactivate: true,
		why: "deactivated: no add-points form, and a departure notification",
	},
	{id: "katherine", name: "Katherine Johnson", email: "katherine@example.com", adds: []int{95}, why: "basic"},
	{id: "alan", name: "Alan Turing", email: "alan@example.com", adds: []int{300, 180}, why: "basic, near gold"},
	{id: "margaret", name: "Margaret Hamilton", email: "margaret@example.com", adds: []int{200, 150}, why: "basic"},
	{id: "donald", name: "Donald Knuth", email: "donald@example.com", adds: []int{400, 50}, why: "basic"},

	// --- gold -----------------------------------------------------------------
	{
		id: "ada", name: "Ada Lovelace", email: "ada@example.com",
		adds: []int{120, 200, 180, 60, 40, 20, 20},
		why:  "ordinary active customer, gold, a few generations in",
	},
	{id: "barbara", name: "Barbara Liskov", email: "barbara@example.com", adds: []int{400, 320}, why: "gold"},
	{id: "dennis", name: "Dennis Ritchie", email: "dennis@example.com", adds: []int{500}, why: "gold"},
	{id: "ken", name: "Ken Thompson", email: "ken@example.com", adds: []int{300, 300}, why: "gold"},
	{id: "bjarne", name: "Bjarne Stroustrup", email: "bjarne@example.com", adds: []int{700}, why: "gold"},
	{id: "guido", name: "Guido van Rossum", email: "guido@example.com", adds: []int{500, 400}, why: "gold"},

	// --- platinum -------------------------------------------------------------
	{
		id: "grace", name: "Grace Hopper", email: "grace@example.com",
		adds: []int{500, 500, 250, 250},
		why:  "top tier: nextTierAt is 0, which the progress bar has to survive",
	},
	{id: "edsger", name: "Edsger Dijkstra", email: "edsger@example.com", adds: []int{600, 600}, why: "platinum"},
	{id: "john", name: "John von Neumann", email: "john@example.com", adds: []int{1000}, why: "platinum"},
	{id: "claude", name: "Claude Shannon", email: "claude@example.com", adds: []int{800, 800}, why: "platinum"},
	{id: "linus", name: "Linus Torvalds", email: "linus@example.com", adds: []int{1000, 500}, why: "platinum"},
	{
		id: "capped", name: "Max Capacity", email: "max@example.com",
		adds: cappedAdds(),
		why:  "just under the points cap, so handler rejections are reachable",
	},
}

// cappedAdds takes a customer to 99,960 points: high enough that any add over
// 40 gets a handler rejection, which -- unlike a validator rejection -- leaves
// an audit row.
func cappedAdds() []int {
	adds := make([]int, 0, 100)
	for i := 0; i < 99; i++ {
		adds = append(adds, 1000)
	}
	return append(adds, 960)
}

func main() {
	base := env("API_BASE", "http://localhost:8081")

	if err := ping(base); err != nil {
		log.Fatalf("no API at %s: %v\nis the stack up? try `make ps`, then `make up`", base, err)
	}

	set := append([]customer{}, seedSet...)

	start := time.Now()
	created, matched, wrong := 0, 0, 0
	for _, c := range set {
		switch status, err := ensure(base, c); {
		case err != nil:
			wrong++
			log.Printf("  %-10s FAILED: %v", c.id, err)
		case status == "":
			created++
			fmt.Printf("  %-10s created   %-4d adds  %s\n", c.id, len(c.adds), c.why)
		default:
			matched++
			fmt.Printf("  %-10s %s\n", c.id, status)
		}
	}

	fmt.Printf("\n%d created, %d already correct, %d wrong, of %d in %s\n",
		created, matched, wrong, len(set), time.Since(start).Round(time.Millisecond))

	if wrong > 0 {
		fmt.Printf("\nSome customers do not match the intended dataset. Deactivation is soft,\n" +
			"so there is no way to reset them through the API. For a clean slate:\n" +
			"  make reset && make seed\n")
		os.Exit(1)
	}

	fmt.Printf("\n  %s/api/customers\n", base)
	fmt.Printf("  make audit ID=ada\n")
	fmt.Printf("  make reap WF=customer-capped     # then `make audit ID=capped` for a truncated log\n")
}

// ensure creates the customer if absent, and otherwise checks the one that is
// already there against what this dataset intends.
//
// Returns an empty status for "created", a description for "already correct",
// and an error when an existing customer does not match.
func ensure(base string, c customer) (string, error) {
	cur, err := fetch(base, c.id)
	switch {
	case err == nil:
		return check(c, cur)
	case !isNotFound(err):
		return "", err
	}

	if err := create(base, c); err != nil {
		return "", err
	}
	return "", nil
}

// check compares an existing customer with what the dataset asks for. It
// deliberately does not repair a mismatch: points only go up
// (FINDINGS.md#points-only-go-up).
func check(c customer, cur customerState) (string, error) {
	want := 0
	for _, a := range c.adds {
		want += a
	}
	wantStatus := "active"
	if c.deactivate {
		wantStatus = "deactivated"
	}

	if cur.Points != want || cur.Status != wantStatus {
		return "", fmt.Errorf("exists with %d points/%s, dataset wants %d/%s",
			cur.Points, cur.Status, want, wantStatus)
	}
	return fmt.Sprintf("already correct at %d points (%s)", cur.Points, cur.Status), nil
}

// customerState is the subset of CustomerResponse the seed checks against.
type customerState struct {
	Points int    `json:"points"`
	Status string `json:"status"`
}

func fetch(base, id string) (customerState, error) {
	var c customerState
	err := do(http.MethodGet, base+"/api/customers/"+id, nil, &c)
	return c, err
}

// isNotFound reports whether the API said the customer does not exist, as
// opposed to being unable to answer.
func isNotFound(err error) bool {
	var he *httpError
	return errors.As(err, &he) && he.status == http.StatusNotFound
}

func create(base string, c customer) error {
	err := do(http.MethodPost, base+"/api/customers", map[string]string{
		"customerId": c.id, "name": c.name, "email": c.email,
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
			return &httpError{resp.StatusCode, e.Error.Code, e.Error.Message}
		}
		return &httpError{resp.StatusCode, "", string(bytes.TrimSpace(raw))}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// httpError carries the status alongside the API's own words, so callers can
// tell "this customer does not exist" from "the API could not answer".
type httpError struct {
	status  int
	code    string
	message string
}

func (e *httpError) Error() string {
	if e.code != "" {
		return fmt.Sprintf("%s (%s)", e.message, e.code)
	}
	return fmt.Sprintf("HTTP %d: %s", e.status, e.message)
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
