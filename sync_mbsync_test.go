package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// These tests execute real mbsync, not a runCmd mock. The minimal IMAP server
// exposes empty mailboxes, duplicate LIST entries and controlled SELECT errors.
// No connection goes to an operator's server or production maildir.
func TestMbsyncDuplicateRecoveryPreservesMailboxFailures(t *testing.T) {
	if _, err := exec.LookPath("mbsync"); err != nil {
		t.Skip("mbsync is not installed")
	}
	version, err := exec.Command("mbsync", "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(string(version), "isync 1.4.") {
		t.Skip("duplicate-list recovery is specific to isync 1.5+")
	}
	for _, tc := range []struct {
		name        string
		mode        int32
		nonempty    bool
		wantFailure bool
	}{
		{"duplicates only", 0, true, false},
		{"listed SELECT denied", 1, true, true},
		{"SELECT denied only during retry with empty local mailbox", 2, false, true},
		{"previously deleted remote mailbox", 3, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var duplicates atomic.Bool
			var mode, otherSelects atomic.Int32
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { ln.Close() })
			go func() {
				for {
					conn, err := ln.Accept()
					if err != nil {
						return
					}
					go func() {
						defer conn.Close()
						_ = conn.SetDeadline(time.Now().Add(time.Minute))
						fmt.Fprint(conn, "* OK IMAP test server ready\r\n")
						scanner := bufio.NewScanner(conn)
						for scanner.Scan() {
							parts := strings.SplitN(scanner.Text(), " ", 3)
							if len(parts) < 2 {
								return
							}
							args := ""
							if len(parts) == 3 {
								args = parts[2]
							}
							tag, command := parts[0], strings.ToUpper(parts[1])
							switch command {
							case "CAPABILITY":
								fmt.Fprint(conn, "* CAPABILITY IMAP4rev1 UIDPLUS\r\n")
							case "LOGIN":
							case "LIST":
								fmt.Fprint(conn, "* LIST (\\HasNoChildren) \"/\" \"INBOX\"\r\n")
								if duplicates.Load() {
									fmt.Fprint(conn, "* LIST (\\HasNoChildren) \"/\" \"INBOX\"\r\n")
								}
								if mode.Load() != 3 {
									fmt.Fprint(conn, "* LIST (\\HasNoChildren) \"/\" \"Other\"\r\n")
								}
							case "SELECT", "EXAMINE":
								if strings.Trim(args, "\"") == "Other" {
									m := mode.Load()
									if m == 1 || m == 3 || (m == 2 && otherSelects.Add(1) > 1) {
										fmt.Fprintf(conn, "%s NO mailbox unavailable\r\n", tag)
										continue
									}
								}
								fmt.Fprint(conn, "* 0 EXISTS\r\n* 0 RECENT\r\n* OK [UIDVALIDITY 1] valid\r\n* OK [UIDNEXT 1] next\r\n")
							case "UID":
								if !strings.HasPrefix(strings.ToUpper(args), "FETCH ") {
									fmt.Fprintf(conn, "%s BAD unsupported UID command\r\n", tag)
									continue
								}
							case "LOGOUT":
								fmt.Fprintf(conn, "* BYE logout\r\n%s OK logout\r\n", tag)
								return
							default:
								fmt.Fprintf(conn, "%s BAD unsupported command\r\n", tag)
								continue
							}
							fmt.Fprintf(conn, "%s OK %s completed\r\n", tag, command)
						}
					}()
				}
			}()

			root := t.TempDir()
			maildir, index := filepath.Join(root, "mail"), filepath.Join(root, "index")
			for _, dir := range []string{filepath.Join(maildir, "home"), index} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			host, portString, _ := net.SplitHostPort(ln.Addr().String())
			port, _ := strconv.Atoi(portString)
			cfg := &Config{Accounts: []Account{{Name: "home", Host: host, Port: port, User: "tester", Password: "secret", TLS: "none", Patterns: []string{"*"}}}}
			config := filepath.Join(root, "mbsyncrc")
			if err := os.WriteFile(config, []byte(genMbsyncrc(cfg, maildir)), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			// Establish real sync state while both LIST entries are unique and
			// both mailboxes can be opened. This is not a completed MCP pass.
			if out, err := execCommand(ctx, "mbsync", "-c", config, "home-full"); err != nil {
				t.Fatalf("initial state: %v\n%s", err, out)
			}
			if tc.nonempty {
				if err := os.WriteFile(filepath.Join(maildir, "home", "Other", "cur", "local:2,S"), []byte("From: fixture@example.org\nSubject: local copy\n\nkeep me\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			duplicates.Store(true)
			mode.Store(tc.mode)
			if tc.mode == 1 {
				// Prove the behavioral difference reported in review, using the
				// actual binary: known-present fails, explicit/possible skips.
				out, err := execCommand(ctx, "mbsync", "-c", config, "home-full")
				if err == nil || !missingNearBox.MatchString(out) || !strings.Contains(out, "far side box Other cannot be opened.") {
					t.Fatalf("normal channel must report both failures: %v\n%s", err, out)
				}
				out, err = execCommand(ctx, "mbsync", "-c", config, "home-full:INBOX,Other")
				if err != nil || !strings.Contains(out, "Warning: channel home-full: far side box Other cannot be opened anymore") {
					t.Fatalf("explicit channel must demonstrate the masked opening error: %v\n%s", err, out)
				}
			}
			s := newSyncer(cfg, maildir, index, config, nil)
			s.timeout = 30 * time.Second
			s.reindex = func(context.Context) (int, error) { return 0, nil }
			_, err = s.Sync(ctx, "home")
			if (err != nil) != tc.wantFailure {
				t.Fatalf("sync error = %v, want failure=%v; status=%+v", err, tc.wantFailure, s.Status()["home"])
			}
			st := s.Status()["home"]
			if tc.wantFailure {
				if st.Complete || !st.LastSync.IsZero() || st.LastError == "" {
					t.Fatalf("skipped mailbox marked complete: %+v", st)
				}
				if _, err := os.Stat(filepath.Join(maildir, "home-recent")); err != nil {
					t.Fatalf("failed recovery must preserve recent store: %v", err)
				}
				if _, err := os.Stat(completeMarker(index, "home")); !os.IsNotExist(err) {
					t.Fatalf("failed recovery must not persist completion: %v", err)
				}
			} else if !st.Complete || st.LastError != "" {
				t.Fatalf("clean recovery did not complete: %+v", st)
			}
		})
	}
}
