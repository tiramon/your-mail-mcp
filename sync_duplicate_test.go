package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const duplicateMailboxError = "Error: channel home-full: near side box Shared/owner@example.org/INBOX cannot be opened anymore."

func TestUniqueChannelMailboxes(t *testing.T) {
	tests := []struct {
		name, listing string
		want          []string
		duplicates    bool
		invalid       bool
	}{
		{"empty", "", nil, false, false},
		{"distinct", "INBOX\nArchive\nArchives\narchive\n", []string{"INBOX", "Archive", "Archives", "archive"}, false, false},
		{"duplicates", "INBOX\nShared/owner@example.org/INBOX\nShared/owner@example.org/INBOX\nShared/owner@example.org/INBOX/Child\n", []string{"INBOX", "Shared/owner@example.org/INBOX", "Shared/owner@example.org/INBOX/Child"}, true, false},
		{"exact names", " Sent Items \nEntwürfe\n Sent Items \nEntwürfe", []string{" Sent Items ", "Entwürfe"}, true, false},
		{"comma", "INBOX\nINBOX\nReports, 2026\n", []string{"INBOX", "Reports, 2026"}, true, true},
		{"carriage return", "INBOX\nINBOX\nBad\rName\n", []string{"INBOX", "Bad\rName"}, true, true},
		{"nul", "INBOX\nINBOX\nBad\x00Name\n", []string{"INBOX", "Bad\x00Name"}, true, true},
		{"empty line", "INBOX\n\nINBOX\n", []string{"INBOX"}, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, duplicates, err := uniqueChannelMailboxes(tt.listing)
			if !reflect.DeepEqual(got, tt.want) || duplicates != tt.duplicates || (err != nil) != tt.invalid {
				t.Fatalf("got %q, duplicates=%v, err=%v; want %q, duplicates=%v, invalid=%v", got, duplicates, err, tt.want, tt.duplicates, tt.invalid)
			}
		})
	}
	if _, _, err := uniqueChannelMailboxes(strings.Repeat("a", 64*1024+1)); err == nil {
		t.Fatal("an oversized override must be refused")
	}
}

func TestSyncFullRecoversDuplicateMailboxListing(t *testing.T) {
	s, _ := testSyncer(t)
	var calls [][]string
	s.runCmd = func(_ context.Context, command string, args ...string) (string, error) {
		if command != "mbsync" {
			t.Fatalf("unexpected command %q", command)
		}
		calls = append(calls, append([]string{}, args...))
		if args[len(args)-1] == "home-full" {
			return duplicateMailboxError, errors.New("mbsync: exit status 1: " + duplicateMailboxError)
		}
		if strings.HasPrefix(args[len(args)-1], "home-full:") {
			return successfulMailboxTrace(strings.Split(strings.TrimPrefix(args[len(args)-1], "home-full:"), ",")), nil
		}
		return "", nil
	}
	listCalls := 0
	s.listCmd = func(_ context.Context, command string, args ...string) (string, error) {
		listCalls++
		if command != "mbsync" || !reflect.DeepEqual(args, []string{"-l", "-c", s.mbsyncConfig, "home-full"}) {
			t.Fatalf("unexpected listing command: %s %q", command, args)
		}
		return "INBOX\nArchive\nArchives\nShared/owner@example.org/INBOX\nShared/owner@example.org/INBOX\nShared/owner@example.org/INBOX/Child\n Sent Items \nEntwürfe\n", nil
	}
	if _, err := s.Sync(context.Background(), "home"); err != nil {
		t.Fatal(err)
	}
	if listCalls != 1 || len(calls) != 3 {
		t.Fatalf("want recent, full, list, one retry; got calls=%q, list calls=%d", calls, listCalls)
	}
	want := "home-full:INBOX,Archive,Archives,Shared/owner@example.org/INBOX,Shared/owner@example.org/INBOX/Child, Sent Items ,Entwürfe"
	if !reflect.DeepEqual(calls[2], []string{"-V", "-c", s.mbsyncConfig, want}) {
		t.Fatalf("retry = %q, want configured full channel with exact unique names", calls[2])
	}
	if st := s.Status()["home"]; !st.Complete || st.LastError != "" || st.Failures != 0 {
		t.Fatalf("recovered pass must count as success: %+v", st)
	}
}

func successfulMailboxTrace(boxes []string) string {
	var out strings.Builder
	for _, box := range boxes {
		out.WriteString("Opening far side box " + box + "...\nSynchronizing...\n")
	}
	return out.String()
}

func TestValidateMailboxRetry(t *testing.T) {
	boxes := []string{"INBOX", "Other"}
	warning := "Warning: channel home-full: far side box Other cannot be opened anymore, and near side box Other is not empty.\n"
	for _, tc := range []struct {
		name, initial, retry string
		wantError            bool
	}{
		{"all synchronized", "", successfulMailboxTrace(boxes), false},
		{"SELECT denied", "", successfulMailboxTrace(boxes[:1]) + "Opening far side box Other...\n" + warning, true},
		{"empty local mailbox removed", "", successfulMailboxTrace(boxes[:1]) + "Opening far side box Other...\nDeleting near side box Other...\n", true},
		{"known remote deletion", warning, successfulMailboxTrace(boxes[:1]) + warning, false},
		{"known warning but unreported skip", warning, successfulMailboxTrace(boxes[:1]), true},
		{"unrecognized output", "", "Channels: 1 Boxes: 2\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateMailboxRetry("home-full", boxes, tc.initial, tc.retry)
			if (err != nil) != tc.wantError {
				t.Fatalf("validation = %v, want error=%v", err, tc.wantError)
			}
		})
	}
}

func TestSyncFullDuplicateRecoveryDoesNotHideOtherFailures(t *testing.T) {
	initialErr := errors.New("mbsync: exit status 1: " + duplicateMailboxError)
	retryErr := errors.New("retry failed")
	listingErr := errors.New("listing failed")
	tests := []struct {
		name, output, listing string
		initial, list, retry  error
		want                  error
		listCalls, syncCalls  int
	}{
		{"healthy", "", "", nil, nil, nil, nil, 0, 1},
		{"authentication", "AUTHENTICATIONFAILED", "", errors.New("authentication failed"), nil, nil, nil, 0, 1},
		{"genuinely missing", duplicateMailboxError, "INBOX\n", initialErr, nil, nil, initialErr, 1, 1},
		{"empty listing", duplicateMailboxError, "", initialErr, nil, nil, initialErr, 1, 1},
		{"listing fails", duplicateMailboxError, "", initialErr, listingErr, nil, initialErr, 1, 1},
		{"ambiguous name", duplicateMailboxError, "INBOX\nINBOX\nReports, 2026\n", initialErr, nil, nil, initialErr, 1, 1},
		{"retry fails", duplicateMailboxError, "INBOX\nINBOX\n", initialErr, nil, retryErr, retryErr, 1, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := testSyncer(t)
			syncCalls, listCalls := 0, 0
			s.runCmd = func(context.Context, string, ...string) (string, error) {
				syncCalls++
				if syncCalls == 1 {
					return tt.output, tt.initial
				}
				return "retry output", tt.retry
			}
			s.listCmd = func(context.Context, string, ...string) (string, error) {
				listCalls++
				return tt.listing, tt.list
			}
			_, err := s.syncFull(context.Background(), "home")
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want original/retry error %v preserved", err, tt.want)
			}
			if (err != nil) != (tt.initial != nil) || syncCalls != tt.syncCalls || listCalls != tt.listCalls {
				t.Fatalf("err=%v, sync calls=%d, list calls=%d", err, syncCalls, listCalls)
			}
		})
	}
}

func TestSyncFullDoesNotRecoverAfterCancellation(t *testing.T) {
	s, _ := testSyncer(t)
	ctx, cancel := context.WithCancel(context.Background())
	s.runCmd = func(context.Context, string, ...string) (string, error) {
		cancel()
		return duplicateMailboxError, context.Canceled
	}
	s.listCmd = func(context.Context, string, ...string) (string, error) {
		t.Fatal("cancelled sync must not open another connection")
		return "", nil
	}
	if _, err := s.syncFull(ctx, "home"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestExecListCommandKeepsDiagnosticsOutOfMailboxNames(t *testing.T) {
	out, err := execListCommand(context.Background(), "sh", "-c", "printf 'INBOX\\nArchive\\n'; printf 'SSLType is deprecated\\n' >&2")
	if err != nil || out != "INBOX\nArchive\n" {
		t.Fatalf("listing includes diagnostics: output=%q, err=%v", out, err)
	}
	const mailbox = "AUTHENTICATE XOAUTH2 Reports"
	out, err = execListCommand(context.Background(), "sh", "-c", "printf '%s\\n' \"$0\"", mailbox)
	if err != nil || out != mailbox+"\n" {
		t.Fatalf("a mailbox name must not be redacted: output=%q, err=%v", out, err)
	}
	const secret = "sensitive-auth-data"
	out, err = execListCommand(context.Background(), "sh", "-c", "printf 'INBOX\\n'; printf '%s\\n' \"AUTHENTICATE XOAUTH2 $0\" >&2; exit 1", secret)
	if err == nil || strings.Contains(out+err.Error(), secret) || !strings.Contains(err.Error(), "AUTHENTICATE XOAUTH2 <authdata>") {
		t.Fatalf("listing error lost diagnostics or exposed auth data: output=%q, err=%v", out, err)
	}
}
