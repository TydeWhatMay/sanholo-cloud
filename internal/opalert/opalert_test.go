package opalert

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lhaig/sanholo-cloud/internal/push"
)

type sent struct{ kind, severity, text string }

type fakeNotifier struct {
	mu     sync.Mutex
	got    []sent
	fail   int // fail the next n calls
	capped int // refuse the next n non-critical calls as over the cap
}

func (f *fakeNotifier) NotifyOperator(kind, severity, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail > 0 {
		f.fail--
		return errors.New("queue full")
	}
	if f.capped > 0 && severity != "critical" {
		f.capped--
		return push.ErrCapped
	}
	f.got = append(f.got, sent{kind, severity, text})
	return nil
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time         { return c.t }
func (c *clock) add(d time.Duration)    { c.t = c.t.Add(d) }
func firing(fp, kind, sum string) Alert { return alert("firing", fp, kind, sum) }
func resolved(fp, kind, sum string) Alert {
	return alert("resolved", fp, kind, sum)
}

func alert(status, fp, kind, sum string) Alert {
	return Alert{
		Status: status, Fingerprint: fp,
		Labels:      map[string]string{"alertname": "X", "kind": kind},
		Annotations: map[string]string{"summary": sum},
	}
}

func newIntake() (*Intake, *fakeNotifier, *clock) {
	f := &fakeNotifier{}
	c := &clock{t: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	return New(f, c.now), f, c
}

func TestParse(t *testing.T) {
	cases := []struct {
		name string
		body string
		ok   bool
	}{
		{"v4", `{"version":"4","status":"firing","alerts":[{"status":"firing","labels":{"alertname":"A"}}]}`, true},
		{"other version", `{"version":"3","alerts":[]}`, false},
		{"no version", `{"alerts":[]}`, false},
		{"not json", `version=4`, false},
		{"empty", ``, false},
		{"too large", `{"version":"4","x":"` + strings.Repeat("a", MaxBody) + `"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.body))
			if (err == nil) != tc.ok {
				t.Fatalf("err = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func TestFiringThenResolved(t *testing.T) {
	in, f, c := newIntake()
	steps := []struct {
		name    string
		advance time.Duration
		alert   Alert
		want    []sent // pushes this step adds
	}{
		{"first firing pushes", 0, firing("fp1", "qm-sealed", "qm is sealed"), []sent{{"qm-sealed", "", "qm is sealed"}}},
		{"repeat within the reminder is quiet", 5 * time.Minute, firing("fp1", "qm-sealed", "qm is sealed"), nil},
		{"one second before the reminder: quiet", Reminder - 5*time.Minute - time.Second, firing("fp1", "qm-sealed", "qm is sealed"), nil},
		{"at the reminder: pushes again", time.Second, firing("fp1", "qm-sealed", "qm is sealed"), []sent{{"qm-sealed", "", "qm is sealed"}}},
		{"resolved pushes once", time.Minute, resolved("fp1", "qm-sealed", "qm is sealed"), []sent{{"qm-sealed", "", "Resolved: qm is sealed"}}},
		{"resolved again is quiet", time.Minute, resolved("fp1", "qm-sealed", "qm is sealed"), nil},
		{"resolved never fired is quiet", 0, resolved("fp2", "disk-full", "disk"), nil},
		{"firing again after resolve pushes", time.Minute, firing("fp1", "qm-sealed", "qm is sealed"), []sent{{"qm-sealed", "", "qm is sealed"}}},
	}
	for _, s := range steps {
		before := len(f.got)
		c.add(s.advance)
		if err := in.Handle(Notification{Version: "4", Alerts: []Alert{s.alert}}); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		got := f.got[before:]
		if fmt.Sprint(got) != fmt.Sprint(s.want) {
			t.Fatalf("%s: pushed %v, want %v", s.name, got, s.want)
		}
	}
	if st := in.Stats(); st.Pushed != 4 || st.Deduped != 4 {
		t.Fatalf("stats = %+v, want pushed 4 deduped 4", st)
	}
}

func TestKindAndSummaryFallbacks(t *testing.T) {
	cases := []struct {
		name      string
		alert     Alert
		kind, txt string
	}{
		{"kind label used", firing("a", "backup-failed", "Backups failed"), "backup-failed", "Backups failed"},
		{"bad kind falls back", firing("b", "Bad Kind!", "s"), "platform", "s"},
		{"no kind falls back", Alert{Status: "firing", Fingerprint: "c", Labels: map[string]string{"alertname": "DiskFull"}}, "platform", "DiskFull"},
		{"nothing at all", Alert{Status: "firing", Fingerprint: "d"}, "platform", "Platform alert"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in, f, _ := newIntake()
			if err := in.Handle(Notification{Alerts: []Alert{tc.alert}}); err != nil {
				t.Fatal(err)
			}
			if len(f.got) != 1 || f.got[0].kind != tc.kind || f.got[0].text != tc.txt {
				t.Fatalf("got %v, want %s %q", f.got, tc.kind, tc.txt)
			}
		})
	}
}

// Without Alertmanager's fingerprint, the labels identify the alert: the
// same labels dedupe, different labels do not.
func TestFingerprintFromLabels(t *testing.T) {
	in, f, _ := newIntake()
	a := Alert{Status: "firing", Labels: map[string]string{"alertname": "Throttled", "tenant": "acme"}}
	b := Alert{Status: "firing", Labels: map[string]string{"alertname": "Throttled", "tenant": "bolt"}}
	_ = in.Handle(Notification{Alerts: []Alert{a, a, b}})
	if len(f.got) != 2 {
		t.Fatalf("pushes = %d, want 2", len(f.got))
	}
}

// A failed push is not remembered, so Alertmanager's retry pushes exactly
// the alert that was missed.
func TestFailedPushIsRetried(t *testing.T) {
	in, f, _ := newIntake()
	f.fail = 1
	n := Notification{Alerts: []Alert{firing("a", "k", "first"), firing("b", "k", "second")}}
	if err := in.Handle(n); err == nil {
		t.Fatal("want an error when a push could not be queued")
	}
	if len(f.got) != 1 || f.got[0].text != "second" {
		t.Fatalf("after the failure: %v", f.got)
	}
	if err := in.Handle(n); err != nil {
		t.Fatal(err)
	}
	if len(f.got) != 2 || f.got[1].text != "first" {
		t.Fatalf("retry pushed %v, want only the missed one", f.got)
	}
	if st := in.Stats(); st.Failed != 1 {
		t.Fatalf("failed = %d", st.Failed)
	}
}

func TestAlertsPerNotificationAreBounded(t *testing.T) {
	in, f, _ := newIntake()
	var alerts []Alert
	for i := range MaxAlerts + 3 {
		alerts = append(alerts, firing(fmt.Sprintf("fp%d", i), "k", "s"))
	}
	_ = in.Handle(Notification{Alerts: alerts})
	if len(f.got) != MaxAlerts {
		t.Fatalf("pushes = %d, want %d", len(f.got), MaxAlerts)
	}
	if st := in.Stats(); st.Truncated != 3 {
		t.Fatalf("truncated = %d, want 3", st.Truncated)
	}
}

// The memory of alerts stays bounded: resolved and day-old entries go.
func TestRememberedIsBounded(t *testing.T) {
	in, _, c := newIntake()
	for i := range maxRemembered {
		_ = in.Handle(Notification{Alerts: []Alert{firing(fmt.Sprintf("old%d", i), "k", "s")}})
	}
	c.add(forget)
	_ = in.Handle(Notification{Alerts: []Alert{firing("new", "k", "s")}})
	in.mu.Lock()
	n := len(in.seen)
	in.mu.Unlock()
	if n != 1 {
		t.Fatalf("remembered %d alerts, want 1", n)
	}
}

// slowNotifier holds every push until release is closed.
type slowNotifier struct {
	fakeNotifier
	entered chan struct{}
	release chan struct{}
}

func (s *slowNotifier) NotifyOperator(kind, severity, text string) error {
	s.entered <- struct{}{}
	<-s.release
	return s.fakeNotifier.NotifyOperator(kind, severity, text)
}

// Two copies of one notification at once (an Alertmanager HA pair) push
// once: the first claims the alert before it sends.
func TestConcurrentCopiesPushOnce(t *testing.T) {
	sn := &slowNotifier{entered: make(chan struct{}, 2), release: make(chan struct{})}
	in := New(sn, nil)
	n := Notification{Alerts: []Alert{firing("fp", "k", "s")}}
	done := make(chan struct{})
	go func() { _ = in.Handle(n); close(done) }()
	<-sn.entered // the first is inside NotifyOperator
	second := make(chan struct{})
	go func() { _ = in.Handle(n); close(second) }()
	select {
	case <-second: // deduplicated without reaching the notifier
	case <-sn.entered:
		close(sn.release)
		t.Fatal("the second copy reached NotifyOperator")
	case <-time.After(5 * time.Second):
		t.Fatal("the second copy blocked")
	}
	close(sn.release)
	<-done
	if len(sn.got) != 1 {
		t.Fatalf("pushes = %d, want 1", len(sn.got))
	}
}

// An alert refused over the operator's budget is neither an error (an
// immediate retry would meet the same cap) nor remembered: the next send of
// it pushes it.
func TestCappedAlertIsPushedOnTheNextSend(t *testing.T) {
	in, f, c := newIntake()
	f.capped = 1
	a := firing("a", "tenant-throttled", "throttled")
	a.Labels["severity"] = "warning"
	n := Notification{Alerts: []Alert{a}}
	if err := in.Handle(n); err != nil {
		t.Fatalf("a capped alert is not a failure: %v", err)
	}
	if len(f.got) != 0 {
		t.Fatalf("pushed while capped: %v", f.got)
	}
	if st := in.Stats(); st.Capped != 1 || st.Pushed != 0 || st.Failed != 0 {
		t.Fatalf("stats = %+v", st)
	}
	c.add(5 * time.Minute) // Alertmanager's group_interval
	if err := in.Handle(n); err != nil {
		t.Fatal(err)
	}
	if len(f.got) != 1 || f.got[0].severity != "warning" {
		t.Fatalf("next send pushed %v, want the capped alert", f.got)
	}
}
