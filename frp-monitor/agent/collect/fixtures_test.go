package collect

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

type fakeSystem struct {
	files map[string]string
	dirs  map[string][]string
	paths map[string]bool
	disks map[string]diskStat
	ips   []interfaceAddress
	now   time.Time
}

func newFake() *fakeSystem {
	return &fakeSystem{
		files: map[string]string{
			"/proc/stat":    "cpu  100 0 50 700 0 0 0 0 0 0\n",
			"/proc/meminfo": "MemTotal: 1000 kB\nMemAvailable: 600 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n",
			"/proc/loadavg": "0.1 0.2 0.3 1/10 100\n", "/proc/uptime": "123.45 100.00\n",
			"/proc/self/mounts":               "/dev/test / ext4 rw 0 0\n",
			"/proc/net/sockstat":              "TCP: inuse 1 orphan 0 tw 2 alloc 4 mem 0\nUDP: inuse 3 mem 0\n",
			"/proc/net/sockstat6":             "TCP6: inuse 4\nUDP6: inuse 5\n",
			"/proc/net/dev":                   netText(map[string][2]uint64{"eth0": {1000, 2000}}),
			"/proc/sys/kernel/random/boot_id": "test-boot\n",
			"/proc/sys/kernel/hostname":       "test-node\n", "/proc/sys/kernel/osrelease": "6.1.0-test\n",
			"/etc/os-release":    "PRETTY_NAME=\"Test Linux\"\n",
			"/proc/cpuinfo":      "processor : 0\nmodel name : Test CPU\nprocessor : 1\nmodel name : Test CPU\n",
			"/proc/net/if_inet6": "",
		},
		dirs: map[string][]string{"/proc": {"1", "7", "net", "self"}}, paths: map[string]bool{}, disks: map[string]diskStat{"/": {Blocks: 1000, Bfree: 400, Frsize: 4096, Bsize: 4096}},
		ips: []interfaceAddress{{Name: "eth0", Up: true, IP: net.ParseIP("192.0.2.10")}}, now: time.Unix(1000, 0),
	}
}
func (f *fakeSystem) deps() dependencies {
	return dependencies{
		read: func(path string) (string, error) {
			value, ok := f.files[path]
			if !ok {
				return "", os.ErrNotExist
			}
			return value, nil
		},
		readDir: func(path string) ([]string, error) {
			value, ok := f.dirs[path]
			if !ok {
				return nil, os.ErrNotExist
			}
			return value, nil
		},
		exists: func(path string) (bool, error) { return f.paths[path], nil },
		statvfs: func(path string) (diskStat, error) {
			value, ok := f.disks[path]
			if !ok {
				return diskStat{}, os.ErrNotExist
			}
			return value, nil
		},
		addresses: func() ([]interfaceAddress, error) { return f.ips, nil }, now: func() time.Time { return f.now }, supported: true, arch: "amd64",
	}
}
func (f *fakeSystem) collector(t *testing.T, iface string) *Collector {
	t.Helper()
	c, err := newCollector(Config{Iface: iface, Version: "test"}, f.deps())
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func netText(values map[string][2]uint64) string {
	var text strings.Builder
	text.WriteString("Inter-| Receive | Transmit\n face |bytes packets errs drop fifo frame compressed multicast|bytes packets errs drop fifo colls carrier compressed\n")
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		v := values[name]
		fmt.Fprintf(&text, "%s: %d 0 0 0 0 0 0 0 %d 0 0 0 0 0 0 0\n", name, v[0], v[1])
	}
	return text.String()
}

type fixtureCase struct {
	ID        string                     `json:"id"`
	Operation string                     `json:"operation"`
	Input     json.RawMessage            `json:"input"`
	Expected  map[string]json.RawMessage `json:"expected"`
}

// The fixture is a single source of truth in tests/fixtures/collect, deliberately
// not embedded or copied. Overlay test scripts supply its absolute path.
func TestPinnedReferenceFixtures(t *testing.T) {
	path := os.Getenv("FRP_MONITOR_COLLECT_FIXTURES")
	if path == "" {
		path = filepath.Join("..", "..", "tests", "fixtures", "collect", "cases.json")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read reference fixtures (set FRP_MONITOR_COLLECT_FIXTURES for overlays): %v", err)
	}
	var file struct {
		Cases []fixtureCase `json:"cases"`
	}
	if err = json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Cases) != 17 {
		t.Fatalf("expected all 17 P0 fixtures, got %d", len(file.Cases))
	}
	for _, test := range file.Cases {
		t.Run(test.ID, func(t *testing.T) {
			fake := newFake()
			c := fake.collector(t, "")
			switch test.Operation {
			case "cpu":
				var input struct{ Previous, Current *string }
				mustJSON(t, test.Input, &input)
				if input.Previous != nil {
					fake.files["/proc/stat"] = *input.Previous
					c.Metrics()
				}
				if input.Current == nil {
					delete(fake.files, "/proc/stat")
				} else {
					fake.files["/proc/stat"] = *input.Current
				}
			case "memory":
				var input struct{ Meminfo string }
				mustJSON(t, test.Input, &input)
				fake.files["/proc/meminfo"] = input.Meminfo
			case "disk":
				var input struct {
					Statvfs []struct {
						Blocks uint64 `json:"f_blocks"`
						Bfree  uint64 `json:"f_bfree"`
						Frsize uint64 `json:"f_frsize"`
						Bsize  uint64 `json:"f_bsize"`
					}
				}
				mustJSON(t, test.Input, &input)
				mounts := ""
				for i, s := range input.Statvfs {
					path := "/disk" + strconv.Itoa(i)
					mounts += fmt.Sprintf("/dev/test%d %s ext4 rw 0 0\n", i, path)
					fake.disks[path] = diskStat{s.Blocks, s.Bfree, s.Frsize, s.Bsize}
				}
				fake.files["/proc/self/mounts"] = mounts
			case "mounts":
				var input struct{ Mounts string }
				mustJSON(t, test.Input, &input)
				got, err := parseMounts(input.Mounts)
				if err != nil {
					t.Fatal(err)
				}
				compareJSON(t, got, test.Expected["mount_points"])
				return
			case "sockets":
				var input struct{ Sockstat, Sockstat6 string }
				mustJSON(t, test.Input, &input)
				fake.files["/proc/net/sockstat"] = input.Sockstat
				fake.files["/proc/net/sockstat6"] = input.Sockstat6
			case "interfaces":
				var input struct {
					Iface      string
					Interfaces []struct {
						Name     string
						Hardware bool
						Type     string
						Uevent   string
						Lower    []string
						Brport   bool
					}
				}
				mustJSON(t, test.Input, &input)
				values := map[string][2]uint64{}
				for _, iface := range input.Interfaces {
					dir := "/sys/class/net/" + iface.Name
					fake.files[dir+"/type"] = iface.Type
					fake.files[dir+"/uevent"] = iface.Uevent
					fake.paths[dir+"/device"] = iface.Hardware
					fake.paths[dir+"/brport"] = iface.Brport
					for _, lower := range iface.Lower {
						fake.dirs[dir] = append(fake.dirs[dir], "lower_"+lower)
					}
					values[iface.Name] = [2]uint64{}
				}
				c = fake.collector(t, input.Iface)
				got, err := c.readNetwork(netText(values))
				if err != nil {
					t.Fatal(err)
				}
				names := make([]string, 0, len(got))
				for name := range got {
					names = append(names, name)
				}
				sort.Strings(names)
				compareJSON(t, names, test.Expected["counted"])
				return
			case "network":
				var input struct {
					Elapsed           *float64 `json:"elapsed_seconds"`
					Previous, Current map[string][2]uint64
				}
				mustJSON(t, test.Input, &input)
				if input.Previous != nil {
					fake.files["/proc/net/dev"] = netText(input.Previous)
					c.Metrics()
				}
				if input.Elapsed != nil {
					fake.now = fake.now.Add(time.Duration(*input.Elapsed * float64(time.Second)))
				}
				fake.files["/proc/net/dev"] = netText(input.Current)
			default:
				t.Fatalf("unhandled production fixture %q", test.Operation)
			}
			metrics := c.Metrics()
			if err := metrics.Validate(); err != nil {
				t.Fatalf("invalid metrics: %v", err)
			}
			wire, err := json.Marshal(metrics)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			mustJSON(t, wire, &fields)
			for name, expected := range test.Expected {
				if name == "reason" {
					continue
				}
				var field struct {
					Value           json.RawMessage
					Quality, Reason string
				}
				mustJSON(t, fields[name], &field)
				if string(expected) == "null" {
					if string(field.Value) != "null" || field.Quality == "ok" {
						t.Errorf("%s: got %s (%s), expected null", name, field.Value, field.Quality)
					}
					if reason := test.Expected["reason"]; reason != nil {
						compareJSON(t, field.Reason, reason)
					}
				} else {
					if field.Quality != "ok" {
						t.Fatalf("%s has quality %s", name, field.Quality)
					}
					compareJSON(t, json.RawMessage(field.Value), expected)
				}
			}
		})
	}
}
func mustJSON(t *testing.T, data []byte, dst any) {
	t.Helper()
	if err := json.Unmarshal(data, dst); err != nil {
		t.Fatal(err)
	}
}
func compareJSON(t *testing.T, got any, want json.RawMessage) {
	t.Helper()
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	a, b := new(big.Rat), new(big.Rat)
	if _, ok := a.SetString(string(data)); ok {
		if _, ok := b.SetString(string(want)); ok {
			if a.Cmp(b) != 0 {
				t.Errorf("got %s, want %s", data, want)
			}
			return
		}
	}
	var actual, expected any
	mustJSON(t, data, &actual)
	mustJSON(t, want, &expected)
	if !reflect.DeepEqual(actual, expected) {
		t.Errorf("got %s, want %s", data, want)
	}
}
