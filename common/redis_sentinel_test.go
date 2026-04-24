package common

import (
	"testing"
)

// parseSentinelURL is the new Sentinel URL parser. Keep these cases close
// to the operator-facing syntax so any regression is obvious.

func TestParseSentinelURL_Minimal(t *testing.T) {
	opt, err := parseSentinelURL("redis-sentinel://10.0.0.1:26379/0?master=mymaster")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if opt.MasterName != "mymaster" {
		t.Errorf("MasterName=%q want mymaster", opt.MasterName)
	}
	if len(opt.SentinelAddrs) != 1 || opt.SentinelAddrs[0] != "10.0.0.1:26379" {
		t.Errorf("SentinelAddrs=%v", opt.SentinelAddrs)
	}
	if opt.DB != 0 {
		t.Errorf("DB=%d want 0", opt.DB)
	}
	if opt.Password != "" {
		t.Errorf("Password=%q want empty", opt.Password)
	}
}

func TestParseSentinelURL_MultiHost_PasswordDB(t *testing.T) {
	raw := "redis-sentinel://:mypw@h1:26379,h2:26379,h3:26379/3?master=cluster-01"
	opt, err := parseSentinelURL(raw)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if opt.MasterName != "cluster-01" {
		t.Errorf("MasterName=%q", opt.MasterName)
	}
	if opt.Password != "mypw" {
		t.Errorf("Password=%q", opt.Password)
	}
	if opt.DB != 3 {
		t.Errorf("DB=%d", opt.DB)
	}
	want := []string{"h1:26379", "h2:26379", "h3:26379"}
	if len(opt.SentinelAddrs) != 3 {
		t.Fatalf("SentinelAddrs=%v", opt.SentinelAddrs)
	}
	for i, a := range opt.SentinelAddrs {
		if a != want[i] {
			t.Errorf("addr[%d]=%q want %q", i, a, want[i])
		}
	}
}

func TestParseSentinelURL_SentinelPasswordQuery(t *testing.T) {
	raw := "redis-sentinel://:masterpw@h1:26379/0?master=m&sentinel_password=sntnlpw"
	opt, err := parseSentinelURL(raw)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if opt.Password != "masterpw" {
		t.Errorf("master Password=%q", opt.Password)
	}
	if opt.SentinelPassword != "sntnlpw" {
		t.Errorf("SentinelPassword=%q", opt.SentinelPassword)
	}
}

func TestParseSentinelURL_MasterRequired(t *testing.T) {
	_, err := parseSentinelURL("redis-sentinel://h1:26379/0")
	if err == nil {
		t.Fatal("expected error when ?master= missing")
	}
}

func TestParseSentinelURL_BadAddr(t *testing.T) {
	_, err := parseSentinelURL("redis-sentinel://not-a-host/0?master=m")
	if err == nil {
		t.Fatal("expected error for addr without :port")
	}
}

func TestParseSentinelURL_EmptyHosts(t *testing.T) {
	_, err := parseSentinelURL("redis-sentinel:///0?master=m")
	if err == nil {
		t.Fatal("expected error for empty hosts")
	}
}

// Env override: SENTINEL_PASSWORD wins over the URL's sentinel_password.
// This lets operators keep Sentinel passwords out of compose files.
func TestParseSentinelURL_EnvOverridesSentinelPassword(t *testing.T) {
	t.Setenv("SENTINEL_PASSWORD", "from-env")
	opt, err := parseSentinelURL("redis-sentinel://h1:26379/0?master=m&sentinel_password=from-url")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if opt.SentinelPassword != "from-env" {
		t.Errorf("env should override url; got %q", opt.SentinelPassword)
	}
}

// Userinfo with no colon is treated as password (compat with older docs
// that use "secret@host" syntax).
func TestParseSentinelURL_UserinfoNoColonIsPassword(t *testing.T) {
	opt, err := parseSentinelURL("redis-sentinel://secret@h1:26379/0?master=m")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if opt.Password != "secret" {
		t.Errorf("Password=%q want secret", opt.Password)
	}
}
