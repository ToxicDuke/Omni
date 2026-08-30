package system

import (
	"fmt"
	"testing"
)

func TestProjectQuotaSetUsesExt4HardLimit(t *testing.T) {
	var calls [][]string
	q := &ProjectQuota{detect: func(string) (string, error) { return "ext4", nil }, run: func(name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		return nil, nil
	}}
	if err := q.Set("/data/abc", "abc", 20*1024*1024*1024); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %#v", calls)
	}
	if got := calls[0]; len(got) != 5 || got[0] != "chattr" || got[1] != "+P" || got[2] != "-p" || got[3] != "891568579" || got[4] != "/data/abc" {
		t.Fatalf("chattr = %#v", got)
	}
	if got := calls[1]; len(got) != 8 || got[0] != "setquota" || got[1] != "-P" || got[2] != "891568579" || got[4] != "20971520K" || got[7] != "/data/abc" {
		t.Fatalf("setquota = %#v", got)
	}
}

func TestProjectQuotaSetUsesXFSHardLimit(t *testing.T) {
	var calls [][]string
	q := &ProjectQuota{detect: func(string) (string, error) { return "xfs", nil }, run: func(name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		return nil, nil
	}}
	if err := q.Set("/data/abc", "abc", 20*1024*1024*1024); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0][0] != "xfs_quota" || calls[0][5] != "limit -p bhard=21474836480b 891568579" || calls[0][6] != "/data/abc" {
		t.Fatalf("calls = %#v", calls)
	}
}

func TestCommandErrorIncludesUtilityOutput(t *testing.T) {
	err := commandError("set XFS quota", "/data/abc", fmt.Errorf("exit status 1"), []byte("XFS quota not enabled\n"))
	if got := err.Error(); got != "project quota: set XFS quota for \"/data/abc\": exit status 1: XFS quota not enabled" {
		t.Fatalf("error = %q", got)
	}
}

func TestProjectQuotaSetUsesBtrfsQgroupLimit(t *testing.T) {
	var calls [][]string
	q := &ProjectQuota{detect: func(string) (string, error) { return "btrfs", nil }, run: func(name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		return nil, nil
	}}
	if err := q.Set("/data/abc", "abc", 20*1024*1024*1024); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0][0] != "btrfs" || calls[0][1] != "qgroup" || calls[0][2] != "limit" || calls[0][3] != "21474836480" || calls[0][4] != "/data/abc" {
		t.Fatalf("calls = %#v", calls)
	}
}

func TestProjectQuotaSetUsesZFSRefquota(t *testing.T) {
	var calls [][]string
	q := &ProjectQuota{detect: func(string) (string, error) { return "zfs", nil }, run: func(name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		if len(args) > 0 && args[0] == "list" {
			return []byte("tank/servers/abc\t/data/abc\n"), nil
		}
		return nil, nil
	}}
	if err := q.Set("/data/abc", "abc", 20*1024*1024*1024); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[1][0] != "zfs" || calls[1][1] != "set" || calls[1][2] != "refquota=21474836480" || calls[1][3] != "tank/servers/abc" {
		t.Fatalf("calls = %#v", calls)
	}
}
