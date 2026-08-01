// Command seed fills a running stack with a demo dataset.
//
// It drives the HTTP API rather than the Temporal client, so seeding exercises
// exactly the path a user does -- including the rollover retry and the error
// mapping. If the API is wrong, the seed notices.
//
//	make seed     # needs `make up`, `make worker`, `make api`
//	make reset    # for a clean slate; see below
//
// Read-then-create, never modify. It looks each customer up first and only
// enrolls the ones that are missing; anyone who already exists is checked
// against the intended balance and left alone. Two reasons for that shape:
//
//   - Deactivation is soft (PLAN.md 3.6), so enrolling an existing customer
//     *reactivates* them with their points intact. A seeder that enrolled
//     blindly would flip the deliberately-deactivated fixture back to active,
//     and then stack a second set of adds on top of the balance it kept.
//   - That is not hypothetical. It is what this program did until the soft
//     deactivation change landed, when `FRESH=1` -- which deactivated and
//     re-enrolled to get a clean slate -- started leaving `ada` at 1280 points
//     and 14 earn events instead of 640 and 7.
//
// So `FRESH=1` is gone: under soft deactivation there is no API call that
// resets a customer, because that is rather the point of it. The only true
// clean slate is deleting the executions, which is `make reset`.
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
// that is the only way points ever enter the system -- there is no back door,
// which is rather the point of the design.
type customer struct {
	id, name, email string
	// adds is the sequence of point-adds to apply, in order. Chosen so tier
	// crossings and continue-as-new boundaries land where the demo wants them.
	adds []int
	// deactivate closes the customer at the end.
	deactivate bool
	why        string
}

// Six active-or-notable customers per tier (basic < 500, gold < 1000,
// platinum ≥ 1000). That fills the tier filter and pushes the unfiltered list
// well past ListLimit so the "Showing 5 of N" notice renders.
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
// 40 gets a *handler* rejection, which -- unlike a validator rejection -- leaves
// an audit row. Reaching it organically takes 100 adds, which is exactly why the
// UI's rejection rendering goes untested without a seeded customer like this.
//
// It also produces ~33 generations, so `make reap WF=customer-capped` gives an
// audit log truncated to a handful of rows out of hundreds.
func cappedAdds() []int {
	adds := make([]int, 0, 100)
	for i := 0; i < 99; i++ {
		adds = append(adds, 1000)
	}
	return append(adds, 960)
}

func main() {
	base := env("API_BASE", "http://localhost:8081")
	if os.Getenv("FRESH") != "" {
		log.Fatal("FRESH=1 no longer does anything: deactivation is soft, so re-enrolling\n" +
			"restores a customer's points rather than resetting them. For a clean slate:\n" +
			"  make reset && make seed")
	}

	if err := ping(base); err != nil {
		log.Fatalf("no API at %s: %v\nis `make api` running (and `make up` and `make worker`)?", base, err)
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

	// Counted rather than assumed. Printing len(set) unconditionally made a run
	// where every customer already existed -- the common case on a second
	// invocation -- look like a complete success, which is the one thing a seed
	// script must never do.
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
// and an error when an existing customer does not match -- which is a real
// failure rather than a skip, because the seed's whole job is to leave the stack
// in a known state and it has just found that it is not in one.
func ensure(base string, c customer) (string, error) {
	cur, err := fetch(base, c.id)
	switch {
	case err == nil:
		return check(c, cur)
	case !isNotFound(err):
		return "", err
	}

	// Absent: enroll and apply the adds.
	if err := create(base, c); err != nil {
		return "", err
	}
	return "", nil
}

// check compares an existing customer with what the dataset asks for.
//
// Deliberately does not try to repair a mismatch. Points only go up (PLAN.md
// 3.1) so a balance that is too high cannot be corrected, and one that is too
// low would need adds whose reasons and timing would not match the rest --
// producing a customer that looks seeded but has an audit log that disagrees.
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
			// A fresh idempotency key per add, as the UI sends. Deterministic
			// here so a re-run is traceable rather than random.
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

// do sends a request and turns a non-2xx into the API's own error message,
// which is more useful than a status code -- and demonstrates that the error
// contract is usable by something other than the UI.
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
// tell "this customer does not exist" from "the API could not answer" -- a
// distinction replace() depends on and a bare error string cannot express.
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
