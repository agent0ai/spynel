package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/extensions"
	"gopkg.in/yaml.v3"
)

const runtimeProcessFixtureEnv = "SPYNEL_RUNTIME_PROCESS_FIXTURE"

func TestRuntimeProcessFixture(t *testing.T) {
	if os.Getenv(runtimeProcessFixtureEnv) != "extension-protocol" {
		return
	}
	_, _ = fmt.Fprintln(os.Stdout, `{"payload":{"text":"protocol-output-must-not-be-logged"}}`)
	_, _ = fmt.Fprintln(os.Stderr, "authorization: Bearer subprocess-secret")
	_, _ = fmt.Fprintln(os.Stderr, "diagnostic-stderr")
	os.Exit(0)
}

func TestProductionSubprocessWiringCapturesDiagnosticStderrWithoutProtocolDuplication(t *testing.T) {
	root := t.TempDir()
	extensionDir := filepath.Join(root, "extensions", "fixture")
	if err := os.MkdirAll(extensionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest, err := yaml.Marshal(extensions.Manifest{Name: "fixture", Hooks: map[string][]string{
		"message.received": {os.Args[0], "-test.run=^TestRuntimeProcessFixture$"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extensionDir, extensions.ManifestName), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(runtimeProcessFixtureEnv, "extension-protocol")
	runtime := NewRuntimeAt(filepath.Join(root, "logs"), "process-fixture")
	defer runtime.Close()
	runner := extensions.Runner{Directory: filepath.Join(root, "extensions"), Timeout: 5 * time.Second, Log: runtime.Writer("extensions")}
	result, err := runner.Run(context.Background(), "message.received", map[string]any{"text": "ordinary user content"})
	if err != nil || result.Payload["text"] != "protocol-output-must-not-be-logged" {
		t.Fatalf("hook result = %#v, %v", result, err)
	}
	stderrMatches := 0
	authorizationMatches := 0
	for _, entry := range runtime.Logs() {
		if strings.Contains(entry.Text, "diagnostic-stderr") {
			stderrMatches++
			if entry.Component != "extensions" {
				t.Fatalf("unsafe or unattributed stderr entry = %#v", entry)
			}
		}
		if entry.Text == "authorization: [REDACTED]" && entry.Component == "extensions" {
			authorizationMatches++
		}
		if strings.Contains(entry.Text, "subprocess-secret") {
			t.Fatalf("authorization credential entered runtime logs: %#v", entry)
		}
		if strings.Contains(entry.Text, "protocol-output-must-not-be-logged") || strings.Contains(entry.Text, "ordinary user content") {
			t.Fatalf("protocol or user content entered runtime logs: %#v", entry)
		}
	}
	if stderrMatches != 1 || authorizationMatches != 1 {
		t.Fatalf("diagnostic stderr captured %d times and redacted authorization %d times, want exactly once each; logs=%#v", stderrMatches, authorizationMatches, runtime.Logs())
	}
}

func TestExtensionInstallWriterRedactsURLUserinfoAcrossRestart(t *testing.T) {
	directory := t.TempDir()
	first := NewRuntimeAt(directory, "install-url-first")
	writer := first.Writer("extension.install")
	_, _ = fmt.Fprintln(writer, "process=git operation=clone stream=stderr truncated=false output=fatal: unable to access 'https://clone-user:LEAKED-URL-PASSWORD@example.com/repo.git/': failure")
	_, _ = fmt.Fprintln(writer, "process=git operation=clone event=exit status=failed exit_code=23")

	assertEvidence := func(entries []LogEntry) {
		t.Helper()
		var diagnostic, exit bool
		for _, entry := range entries {
			if entry.Component != "extension.install" {
				continue
			}
			if strings.Contains(entry.Text, "LEAKED-URL-PASSWORD") || strings.Contains(entry.Text, "clone-user") {
				t.Fatalf("URL userinfo entered retained logs: %#v", entry)
			}
			if strings.Contains(entry.Text, "https://[REDACTED]@example.com/repo.git/") && strings.Contains(entry.Text, "fatal: unable to access") {
				diagnostic = true
			}
			if strings.Contains(entry.Text, "process=git") && strings.Contains(entry.Text, "status=failed") && strings.Contains(entry.Text, "exit_code=23") {
				exit = true
			}
		}
		if !diagnostic || !exit {
			t.Fatalf("install diagnostic=%t exit=%t; logs=%#v", diagnostic, exit, entries)
		}
	}
	assertEvidence(first.Logs())
	first.Close()

	paths, err := filepath.Glob(filepath.Join(directory, "runtime-*.jsonl"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("persistent log paths = %v, %v", paths, err)
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "LEAKED-URL-PASSWORD") || strings.Contains(string(data), "clone-user") {
			t.Fatalf("URL userinfo persisted in %s", filepath.Base(path))
		}
	}

	restored := NewRuntimeAt(directory, "install-url-restored")
	defer restored.Close()
	assertEvidence(restored.Logs())
}

func TestRuntimeLogPersistsRestoresRedactsAndToleratesPartialTail(t *testing.T) {
	directory := t.TempDir()
	first := NewRuntimeAt(directory, "first")
	first.LogEvent("error", "telegram", "connect_failed", "authorization: Bearer top-secret\napi_key=hunter2\n\"bot_token\":\"json-secret\"\nGITHUB_TOKEN=env-secret\n\"password\":\"two words\"")
	first.Close()

	paths, err := filepath.Glob(filepath.Join(directory, "runtime-*.jsonl"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("persistent log paths = %v, %v", paths, err)
	}
	file, err := os.OpenFile(paths[len(paths)-1], os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.WriteString(`{"at":`)
	_ = file.Close()

	second := NewRuntimeAt(directory, "second")
	defer second.Close()
	logs := second.Logs()
	if len(logs) < 4 {
		t.Fatalf("restored logs = %#v", logs)
	}
	var found bool
	for _, entry := range logs {
		if entry.Event == "connect_failed" {
			found = true
			if strings.Contains(entry.Text, "top-secret") || strings.Contains(entry.Text, "hunter2") || strings.Contains(entry.Text, "json-secret") || strings.Contains(entry.Text, "env-secret") || strings.Contains(entry.Text, "two words") || strings.Count(entry.Text, "[REDACTED]") != 5 {
				t.Fatalf("secret was not centrally redacted: %q", entry.Text)
			}
		}
	}
	if !found {
		t.Fatalf("restart did not restore attributed error: %#v", logs)
	}
}

func TestRuntimeLogRedactsCompleteAuthorizationValues(t *testing.T) {
	tests := []struct {
		name    string
		message string
		secret  string
	}{
		{name: "bearer", message: "Authorization: Bearer bearer-secret", secret: "bearer-secret"},
		{name: "basic", message: "Authorization: Basic dXNlcjpwYXNz", secret: "dXNlcjpwYXNz"},
		{name: "token", message: "Authorization: token token-secret", secret: "token-secret"},
		{name: "unqualified", message: "Authorization=opaque-secret", secret: "opaque-secret"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := boundAndRedactLogText(test.message)
			if strings.Contains(got, test.secret) || strings.Count(got, "[REDACTED]") != 1 {
				t.Fatalf("redacted authorization = %q", got)
			}
		})
	}
}

func TestRuntimeLogRedactsBearerCredentialsWithoutAuthorizationHeader(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    string
	}{
		{name: "auth alias", message: "auth: Bearer LEAKED-BEARER-CREDENTIAL\nstatus: retrying", want: "auth: [REDACTED]\nstatus: retrying"},
		{name: "quoted auth alias", message: `{"auth":"Bearer LEAKED-BEARER-CREDENTIAL","status":"retrying"}`, want: `{"auth":"[REDACTED]","status":"retrying"}`},
		{name: "standalone fragment", message: "request rejected: Bearer LEAKED-BEARER-CREDENTIAL; retrying", want: "request rejected: Bearer [REDACTED]; retrying"},
		{name: "standalone jwt", message: "credential Bearer eyJhbGciOiJIUzI1NiJ9.payload_signature=", want: "credential Bearer [REDACTED]"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := boundAndRedactLogText(test.message)
			if got != test.want || strings.Contains(got, "LEAKED-BEARER-CREDENTIAL") {
				t.Fatalf("redacted bearer credential = %q, want %q", got, test.want)
			}
		})
	}
}

func TestRuntimeLogRedactsCompleteUnquotedMultiwordCredentialValues(t *testing.T) {
	tests := []struct {
		name    string
		message string
		secret  string
		want    string
	}{
		{
			name:    "yaml value preserves next line",
			message: "password: correct horse battery staple\nconnection retry scheduled",
			secret:  "correct horse battery staple",
			want:    "password: [REDACTED]\nconnection retry scheduled",
		},
		{
			name:    "ambiguous same-line field remains credential data",
			message: "api_key=alpha beta gamma status=retrying",
			secret:  "alpha beta gamma status=retrying",
			want:    "api_key=[REDACTED]",
		},
		{
			name:    "environment punctuation remains credential data",
			message: "SERVICE_PASSWORD=space separated value, retry=2",
			secret:  "space separated value, retry=2",
			want:    "SERVICE_PASSWORD=[REDACTED]",
		},
		{
			name:    "yaml comment is preserved",
			message: "password: correct horse battery staple # connection retry scheduled",
			secret:  "correct horse battery staple",
			want:    "password: [REDACTED] # connection retry scheduled",
		},
		{
			name:    "commas and semicolons remain credential data",
			message: "password: alpha,beta; gamma delta",
			secret:  "alpha,beta; gamma delta",
			want:    "password: [REDACTED]",
		},
		{
			name:    "url-like content remains credential data",
			message: "password: alpha https://credential.example/private",
			secret:  "alpha https://credential.example/private",
			want:    "password: [REDACTED]",
		},
		{
			name:    "colon-bearing content remains credential data",
			message: "password: correct horse:battery staple",
			secret:  "correct horse:battery staple",
			want:    "password: [REDACTED]",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := boundAndRedactLogText(test.message)
			if got != test.want || strings.Contains(got, test.secret) {
				t.Fatalf("redacted credential = %q, want %q", got, test.want)
			}
		})
	}
}

func TestRuntimeLogRedactsCompleteQuotedCredentialValuesWithEscapes(t *testing.T) {
	tests := []struct {
		name    string
		message string
		secret  string
		want    string
	}{
		{
			name:    "escaped quote",
			message: `{"password":"safe-prefix\"LEAKED-SECRET","status":"retrying"}`,
			secret:  `safe-prefix\"LEAKED-SECRET`,
			want:    `{"password":"[REDACTED]","status":"retrying"}`,
		},
		{
			name:    "escaped backslash",
			message: `{"api_key":"safe-prefix\\LEAKED-SECRET","status":"retrying"}`,
			secret:  `safe-prefix\\LEAKED-SECRET`,
			want:    `{"api_key":"[REDACTED]","status":"retrying"}`,
		},
		{
			name:    "unicode escape",
			message: `{"secret":"safe-prefix\u0022LEAKED-SECRET","status":"retrying"}`,
			secret:  `safe-prefix\u0022LEAKED-SECRET`,
			want:    `{"secret":"[REDACTED]","status":"retrying"}`,
		},
		{
			name:    "doubled single quote",
			message: `{'password':'safe-prefix''LEAKED-SECRET','status':'retrying'}`,
			secret:  `safe-prefix''LEAKED-SECRET`,
			want:    `{'password':'[REDACTED]','status':'retrying'}`,
		},
		{
			name:    "bare yaml key with doubled single quote",
			message: `password: 'safe-prefix''LEAKED-SECRET'`,
			secret:  `safe-prefix''LEAKED-SECRET`,
			want:    `password: '[REDACTED]'`,
		},
		{
			name:    "environment assignment with doubled single quote",
			message: `SERVICE_PASSWORD='safe-prefix''LEAKED-SECRET'`,
			secret:  `safe-prefix''LEAKED-SECRET`,
			want:    `SERVICE_PASSWORD='[REDACTED]'`,
		},
		{
			name:    "double quoted key with single quoted value",
			message: `{"password":'safe-prefix''LEAKED-SECRET',"status":"retrying"}`,
			secret:  `safe-prefix''LEAKED-SECRET`,
			want:    `{"password":'[REDACTED]',"status":"retrying"}`,
		},
		{
			name:    "single quoted key with double quoted value",
			message: `{'password':"safe-prefix\"LEAKED-SECRET",'status':'retrying'}`,
			secret:  `safe-prefix\"LEAKED-SECRET`,
			want:    `{'password':"[REDACTED]",'status':'retrying'}`,
		},
		{
			name:    "single quoted backslash preserves adjacent field",
			message: `{'password':'safe-prefix\','status':'retrying'}`,
			secret:  `safe-prefix\`,
			want:    `{'password':'[REDACTED]','status':'retrying'}`,
		},
		{
			name:    "URL userinfo preserves host and diagnostic",
			message: "fatal: unable to access 'https://clone-user:LEAKED-URL-PASSWORD@example.com/repo.git/': failure",
			secret:  `LEAKED-URL-PASSWORD`,
			want:    "fatal: unable to access 'https://[REDACTED]@example.com/repo.git/': failure",
		},
		{
			name:    "quoted extended secret key",
			message: `{"client_secret":"LEAKED-SECRET","status":"retrying"}`,
			secret:  `LEAKED-SECRET`,
			want:    `{"client_secret":"[REDACTED]","status":"retrying"}`,
		},
		{
			name:    "quoted credentials key",
			message: `{"credentials":"LEAKED-SECRET","status":"retrying"}`,
			secret:  `LEAKED-SECRET`,
			want:    `{"credentials":"[REDACTED]","status":"retrying"}`,
		},
		{
			name:    "quoted private key",
			message: `{"private_key":"LEAKED-SECRET","status":"retrying"}`,
			secret:  `LEAKED-SECRET`,
			want:    `{"private_key":"[REDACTED]","status":"retrying"}`,
		},
		{
			name:    "quoted passphrase",
			message: `{"passphrase":"LEAKED-SECRET","status":"retrying"}`,
			secret:  `LEAKED-SECRET`,
			want:    `{"passphrase":"[REDACTED]","status":"retrying"}`,
		},
		{
			name:    "quoted namespaced signing key",
			message: `{"service.signing-key":"LEAKED-SECRET","status":"retrying"}`,
			secret:  `LEAKED-SECRET`,
			want:    `{"service.signing-key":"[REDACTED]","status":"retrying"}`,
		},
		{
			name:    "quoted extended password key",
			message: `{"SERVICE_PASSWORD":'LEAKED-SECRET',"status":"retrying"}`,
			secret:  `LEAKED-SECRET`,
			want:    `{"SERVICE_PASSWORD":'[REDACTED]',"status":"retrying"}`,
		},
		{
			name:    "quoted extended bot token key",
			message: `{'TELEGRAM_BOT_TOKEN':'LEAKED-SECRET','status':'retrying'}`,
			secret:  `LEAKED-SECRET`,
			want:    `{'TELEGRAM_BOT_TOKEN':'[REDACTED]','status':'retrying'}`,
		},
		{
			name:    "quoted dotted bot token key",
			message: `{"telegram.bot_token":"LEAKED-SECRET","status":"retrying"}`,
			secret:  `LEAKED-SECRET`,
			want:    `{"telegram.bot_token":"[REDACTED]","status":"retrying"}`,
		},
		{
			name:    "quoted hyphenated bot token key",
			message: `{"telegram-bot-token":'LEAKED-SECRET',"status":"retrying"}`,
			secret:  `LEAKED-SECRET`,
			want:    `{"telegram-bot-token":'[REDACTED]',"status":"retrying"}`,
		},
		{
			name:    "quoted prefixed authorization key",
			message: `{"proxy-authorization":"Basic LEAKED-SECRET","status":"retrying"}`,
			secret:  `LEAKED-SECRET`,
			want:    `{"proxy-authorization":"[REDACTED]","status":"retrying"}`,
		},
		{
			name:    "unicode escaped password key",
			message: `{"pass\u0077ord":"LEAKED-SECRET","status":"retrying"}`,
			secret:  `LEAKED-SECRET`,
			want:    `{"pass\u0077ord":"[REDACTED]","status":"retrying"}`,
		},
		{
			name:    "unicode escaped bot token key",
			message: `{"bot\u005ftoken":'LEAKED-SECRET',"status":"retrying"}`,
			secret:  `LEAKED-SECRET`,
			want:    `{"bot\u005ftoken":'[REDACTED]',"status":"retrying"}`,
		},
		{
			name:    "unicode escaped authorization key",
			message: `{"\u0061uthorization":"Bearer LEAKED-SECRET","status":"retrying"}`,
			secret:  `LEAKED-SECRET`,
			want:    `{"\u0061uthorization":"[REDACTED]","status":"retrying"}`,
		},
		{
			name:    "unicode escaped key with plain scalar",
			message: "\"pass\\u0077ord\": correct horse battery staple\nstatus: retrying",
			secret:  `correct horse battery staple`,
			want:    "\"pass\\u0077ord\": [REDACTED]\nstatus: retrying",
		},
		{
			name:    "namespaced key with plain scalar",
			message: "\"telegram.bot_token\": 123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZ_abcd\nstatus: retrying",
			secret:  `123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZ_abcd`,
			want:    "\"telegram.bot_token\": [REDACTED]\nstatus: retrying",
		},
		{
			name:    "bare credentials preserves following diagnostic",
			message: "credentials: safe-prefix LEAKED-SECRET\nstatus: retrying",
			secret:  `safe-prefix LEAKED-SECRET`,
			want:    "credentials: [REDACTED]\nstatus: retrying",
		},
		{
			name:    "bare client credential preserves following diagnostic",
			message: "client_credential: safe-prefix LEAKED-SECRET\nstatus: retrying",
			secret:  `safe-prefix LEAKED-SECRET`,
			want:    "client_credential: [REDACTED]\nstatus: retrying",
		},
		{
			name:    "environment private key preserves following diagnostic",
			message: "PRIVATE_KEY=safe-prefix LEAKED-SECRET\nstatus: retrying",
			secret:  `safe-prefix LEAKED-SECRET`,
			want:    "PRIVATE_KEY=[REDACTED]\nstatus: retrying",
		},
		{
			name:    "bare passphrase preserves following diagnostic",
			message: "passphrase: safe-prefix LEAKED-SECRET\nstatus: retrying",
			secret:  `safe-prefix LEAKED-SECRET`,
			want:    "passphrase: [REDACTED]\nstatus: retrying",
		},
		{
			name:    "environment signing key preserves following diagnostic",
			message: "SIGNING_KEY=safe-prefix LEAKED-SECRET\nstatus: retrying",
			secret:  `safe-prefix LEAKED-SECRET`,
			want:    "SIGNING_KEY=[REDACTED]\nstatus: retrying",
		},
		{
			name:    "bare dotted encryption key preserves following diagnostic",
			message: "service.encryption.key: safe-prefix LEAKED-SECRET\nstatus: retrying",
			secret:  `safe-prefix LEAKED-SECRET`,
			want:    "service.encryption.key: [REDACTED]\nstatus: retrying",
		},
		{
			name:    "space separated ssh key preserves following diagnostic",
			message: "ssh key: safe-prefix LEAKED-SECRET\nstatus: retrying",
			secret:  `safe-prefix LEAKED-SECRET`,
			want:    "ssh key: [REDACTED]\nstatus: retrying",
		},
		{
			name:    "bare dotted private key preserves following diagnostic",
			message: "private.key: safe-prefix LEAKED-SECRET\nstatus: retrying",
			secret:  `safe-prefix LEAKED-SECRET`,
			want:    "private.key: [REDACTED]\nstatus: retrying",
		},
		{
			name:    "namespaced dotted private key preserves following diagnostic",
			message: "tls.private.key: safe-prefix LEAKED-SECRET\nstatus: retrying",
			secret:  `safe-prefix LEAKED-SECRET`,
			want:    "tls.private.key: [REDACTED]\nstatus: retrying",
		},
		{
			name:    "space separated private key preserves following diagnostic",
			message: "private key: safe-prefix LEAKED-SECRET\nstatus: retrying",
			secret:  `safe-prefix LEAKED-SECRET`,
			want:    "private key: [REDACTED]\nstatus: retrying",
		},
		{
			name:    "tab separated private key preserves following diagnostic",
			message: "private\tkey: safe-prefix LEAKED-SECRET\nstatus: retrying",
			secret:  `safe-prefix LEAKED-SECRET`,
			want:    "private    key: [REDACTED]\nstatus: retrying",
		},
		{
			name:    "pem private key consumes complete credential",
			message: "private_key: -----BEGIN PRIVATE KEY-----\nLEAKED-SECRET\n-----END PRIVATE KEY-----\nstatus: retrying",
			secret:  `LEAKED-SECRET`,
			want:    "private_key: [REDACTED]\nstatus: retrying",
		},
		{
			name:    "commented pem header consumes complete credential",
			message: "private_key: -----BEGIN PRIVATE KEY----- # generated\nLEAKED-SECRET\n-----END PRIVATE KEY-----\nstatus: retrying",
			secret:  `LEAKED-SECRET`,
			want:    "private_key: [REDACTED] # generated\nstatus: retrying",
		},
		{
			name:    "commented pem footer preserves following diagnostic",
			message: "private_key: -----BEGIN PRIVATE KEY-----\nLEAKED-SECRET\n-----END PRIVATE KEY----- # complete\nstatus: retrying",
			secret:  `LEAKED-SECRET`,
			want:    "private_key: [REDACTED] # complete\nstatus: retrying",
		},
		{
			name:    "structured credentials preserve adjacent diagnostic",
			message: `{"credentials":{"value":"LEAKED-SECRET"},"status":"retrying"}`,
			secret:  `LEAKED-SECRET`,
			want:    `{"credentials":[REDACTED],"status":"retrying"}`,
		},
		{
			name:    "whatsapp identity and noise keys preserve adjacent diagnostic",
			message: `{"signedIdentityKey":{"public":"PUBLIC-DATA","private":"LEAKED-SECRET"},"noiseKey":{"private":"SECOND-WA-SECRET"},"status":"retrying"}`,
			secret:  `LEAKED-SECRET`,
			want:    `{"signedIdentityKey":[REDACTED],"noiseKey":[REDACTED],"status":"retrying"}`,
		},
		{
			name:    "unknown key pair redacts nested private field",
			message: `{"customKeyPair":{"public":"PUBLIC-DATA","private":"LEAKED-SECRET"},"status":"retrying"}`,
			secret:  `LEAKED-SECRET`,
			want:    `{"customKeyPair":{"public":"PUBLIC-DATA","private":"[REDACTED]"},"status":"retrying"}`,
		},
		{
			name:    "whatsmeow json key pair priv member preserves public and status",
			message: `{"IdentityKey":{"Priv":"LEAKED-ACTUAL-WA-PRIVATE","Pub":"PUBLIC-DATA"},"status":"retrying"}`,
			secret:  `LEAKED-ACTUAL-WA-PRIVATE`,
			want:    `{"IdentityKey":{"Priv":"[REDACTED]","Pub":"PUBLIC-DATA"},"status":"retrying"}`,
		},
		{
			name:    "whatsmeow fmt key pair priv member preserves public and status",
			message: `Device{IdentityKey:&{Priv:LEAKED-FMT-WA-PRIVATE Pub:PUBLIC-DATA} status:retrying}`,
			secret:  `LEAKED-FMT-WA-PRIVATE`,
			want:    `Device{IdentityKey:&{Priv:[REDACTED] Pub:PUBLIC-DATA} status:retrying}`,
		},
		{
			name:    "bare whatsapp key preserves following diagnostic",
			message: "signedIdentityKey: {private: LEAKED-SECRET}\nstatus: retrying",
			secret:  `LEAKED-SECRET`,
			want:    "signedIdentityKey: [REDACTED]\nstatus: retrying",
		},
		{
			name:    "single quoted extended key with plain scalar",
			message: "'client_secret': safe-prefix''LEAKED-SECRET # retry credential\nstatus: retrying",
			secret:  `safe-prefix''LEAKED-SECRET`,
			want:    "'client_secret': [REDACTED] # retry credential\nstatus: retrying",
		},
		{
			name:    "unicode escaped key with yaml literal block",
			message: "\"pass\\u0077ord\": |\n  safe-prefix LEAKED-SECRET\nstatus: retrying",
			secret:  `safe-prefix LEAKED-SECRET`,
			want:    "\"pass\\u0077ord\": [REDACTED]\nstatus: retrying",
		},
		{
			name:    "bare key with yaml folded block",
			message: "password: >-\n  safe-prefix LEAKED-SECRET\n  credential continuation\nstatus: retrying",
			secret:  `safe-prefix LEAKED-SECRET`,
			want:    "password: [REDACTED]\nstatus: retrying",
		},
		{
			name:    "multiline single quoted scalar",
			message: "password: 'safe-prefix\n  LEAKED-SECRET'\nstatus: retrying",
			secret:  `LEAKED-SECRET`,
			want:    "password: '[REDACTED]'\nstatus: retrying",
		},
		{
			name:    "multiline single quoted scalar with blank line",
			message: "password: 'safe-prefix\n\n  LEAKED-SECRET'\nstatus: retrying",
			secret:  `LEAKED-SECRET`,
			want:    "password: '[REDACTED]'\nstatus: retrying",
		},
		{
			name:    "multiline double quoted scalar with crlf blank line",
			message: "password: \"safe-prefix\r\n\r\n  LEAKED-SECRET\"\r\nstatus: retrying",
			secret:  `LEAKED-SECRET`,
			want:    "password: \"[REDACTED]\"\nstatus: retrying",
		},
		{
			name:    "nested block preserves sibling",
			message: "config:\n  password: |\n    LEAKED-SECRET\n  status: retrying\nnext: ok",
			secret:  `LEAKED-SECRET`,
			want:    "config:\n  password: [REDACTED]\n  status: retrying\nnext: ok",
		},
		{
			name:    "nested block crosses whitespace only line",
			message: "config:\n  password: |\n    safe-prefix\n  \n    LEAKED-SECRET\n  status: retrying",
			secret:  `LEAKED-SECRET`,
			want:    "config:\n  password: [REDACTED]\n  status: retrying",
		},
		{
			name:    "tagged literal block",
			message: "password: !!str |\n  LEAKED-SECRET\nstatus: retrying",
			secret:  `LEAKED-SECRET`,
			want:    "password: [REDACTED]\nstatus: retrying",
		},
		{
			name:    "anchored folded block",
			message: "password: &credential >- # private\n  LEAKED-SECRET\nstatus: retrying",
			secret:  `LEAKED-SECRET`,
			want:    "password: [REDACTED]\nstatus: retrying",
		},
		{
			name:    "unterminated value fails closed at line boundary",
			message: "{\"bot_token\":\"safe-prefix\\\"LEAKED-SECRET\nconnection retry scheduled",
			secret:  `safe-prefix\"LEAKED-SECRET`,
			want:    "{\"bot_token\":\"[REDACTED]\nconnection retry scheduled",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := boundAndRedactLogText(test.message)
			if got != test.want || strings.Contains(got, test.secret) {
				t.Fatalf("redacted credential = %q, want %q", got, test.want)
			}
		})
	}

	directory := t.TempDir()
	runtime := NewRuntimeAt(directory, "quoted-escape-redaction")
	cases := []struct {
		event   string
		message string
		want    string
	}{
		{event: "quoted_secret", message: `{"password":"safe-prefix\"LEAKED-SECRET"}`, want: `{"password":"[REDACTED]"}`},
		{event: "single_quoted_secret", message: `{'password':'safe-prefix''LEAKED-SECRET','status':'retrying'}`, want: `{'password':'[REDACTED]','status':'retrying'}`},
		{event: "yaml_quoted_secret", message: `password: 'safe-prefix''LEAKED-SECRET'`, want: `password: '[REDACTED]'`},
		{event: "environment_quoted_secret", message: `SERVICE_PASSWORD='safe-prefix''LEAKED-SECRET'`, want: `SERVICE_PASSWORD='[REDACTED]'`},
		{event: "mixed_quoted_secret", message: `{"password":'safe-prefix''LEAKED-SECRET',"status":"retrying"}`, want: `{"password":'[REDACTED]',"status":"retrying"}`},
		{event: "extended_quoted_secret", message: `{'TELEGRAM_BOT_TOKEN':'safe-prefix''LEAKED-SECRET','status':'retrying'}`, want: `{'TELEGRAM_BOT_TOKEN':'[REDACTED]','status':'retrying'}`},
		{event: "credentials_secret", message: "credentials: safe-prefix LEAKED-SECRET\nstatus: retrying", want: "credentials: [REDACTED]\nstatus: retrying"},
		{event: "client_credential_secret", message: "client_credential: safe-prefix LEAKED-SECRET\nstatus: retrying", want: "client_credential: [REDACTED]\nstatus: retrying"},
		{event: "private_key_secret", message: "private_key: safe-prefix LEAKED-SECRET\nstatus: retrying", want: "private_key: [REDACTED]\nstatus: retrying"},
		{event: "passphrase_secret", message: "passphrase: safe-prefix LEAKED-SECRET\nstatus: retrying", want: "passphrase: [REDACTED]\nstatus: retrying"},
		{event: "signing_key_secret", message: `{"service.signing-key":"safe-prefix LEAKED-SECRET","status":"retrying"}`, want: `{"service.signing-key":"[REDACTED]","status":"retrying"}`},
		{event: "encryption_key_secret", message: "service.encryption.key: safe-prefix LEAKED-SECRET\nstatus: retrying", want: "service.encryption.key: [REDACTED]\nstatus: retrying"},
		{event: "ssh_key_secret", message: "ssh key: safe-prefix LEAKED-SECRET\nstatus: retrying", want: "ssh key: [REDACTED]\nstatus: retrying"},
		{event: "dotted_private_key_secret", message: "private.key: safe-prefix LEAKED-SECRET\nstatus: retrying", want: "private.key: [REDACTED]\nstatus: retrying"},
		{event: "namespaced_private_key_secret", message: "tls.private.key: safe-prefix LEAKED-SECRET\nstatus: retrying", want: "tls.private.key: [REDACTED]\nstatus: retrying"},
		{event: "spaced_private_key_secret", message: "private key: safe-prefix LEAKED-SECRET\nstatus: retrying", want: "private key: [REDACTED]\nstatus: retrying"},
		{event: "tabbed_private_key_secret", message: "private\tkey: safe-prefix LEAKED-SECRET\nstatus: retrying", want: "private    key: [REDACTED]\nstatus: retrying"},
		{event: "pem_private_key_secret", message: "private_key: -----BEGIN PRIVATE KEY-----\nLEAKED-SECRET\n-----END PRIVATE KEY-----\nstatus: retrying", want: "private_key: [REDACTED]\nstatus: retrying"},
		{event: "commented_pem_private_key_secret", message: "private_key: -----BEGIN PRIVATE KEY----- # generated\nLEAKED-SECRET\n-----END PRIVATE KEY-----\nstatus: retrying", want: "private_key: [REDACTED] # generated\nstatus: retrying"},
		{event: "commented_pem_footer_secret", message: "private_key: -----BEGIN PRIVATE KEY-----\nLEAKED-SECRET\n-----END PRIVATE KEY----- # complete\nstatus: retrying", want: "private_key: [REDACTED] # complete\nstatus: retrying"},
		{event: "structured_credentials_secret", message: `{"credentials":{"value":"LEAKED-SECRET"},"status":"retrying"}`, want: `{"credentials":[REDACTED],"status":"retrying"}`},
		{event: "whatsapp_identity_noise_secret", message: `{"signedIdentityKey":{"public":"PUBLIC-DATA","private":"LEAKED-SECRET"},"noiseKey":{"private":"SECOND-WA-SECRET"},"status":"retrying"}`, want: `{"signedIdentityKey":[REDACTED],"noiseKey":[REDACTED],"status":"retrying"}`},
		{event: "whatsapp_signed_prekey_secret", message: `{"signedPreKey":{"keyPair":{"public":"PUBLIC-DATA","private":"LEAKED-SECRET"},"signature":"SECOND-WA-SECRET"},"status":"retrying"}`, want: `{"signedPreKey":[REDACTED],"status":"retrying"}`},
		{event: "whatsapp_unknown_private_secret", message: `{"customKeyPair":{"public":"PUBLIC-DATA","private":"LEAKED-SECRET"},"status":"retrying"}`, want: `{"customKeyPair":{"public":"PUBLIC-DATA","private":"[REDACTED]"},"status":"retrying"}`},
		{event: "whatsmeow_identity_priv_secret", message: `{"IdentityKey":{"Priv":"LEAKED-ACTUAL-WA-PRIVATE","Pub":"PUBLIC-DATA"},"status":"retrying"}`, want: `{"IdentityKey":{"Priv":"[REDACTED]","Pub":"PUBLIC-DATA"},"status":"retrying"}`},
		{event: "whatsmeow_noise_priv_secret", message: `Device{NoiseKey:&{Priv:LEAKED-FMT-WA-PRIVATE Pub:PUBLIC-DATA} status:retrying}`, want: `Device{NoiseKey:&{Priv:[REDACTED] Pub:PUBLIC-DATA} status:retrying}`},
		{event: "namespaced_quoted_secret", message: `{"telegram.bot_token":'safe-prefix''LEAKED-SECRET',"status":"retrying"}`, want: `{"telegram.bot_token":'[REDACTED]',"status":"retrying"}`},
		{event: "prefixed_authorization", message: `{"proxy-authorization":"Basic LEAKED-SECRET","status":"retrying"}`, want: `{"proxy-authorization":"[REDACTED]","status":"retrying"}`},
		{event: "auth_alias", message: "auth: Bearer LEAKED-SECRET\nstatus: retrying", want: "auth: [REDACTED]\nstatus: retrying"},
		{event: "standalone_bearer", message: "request rejected: Bearer LEAKED-SECRET; retrying", want: "request rejected: Bearer [REDACTED]; retrying"},
		{event: "url_userinfo", message: "fatal: unable to access 'https://clone-user:LEAKED-URL-PASSWORD@example.com/repo.git/': failure", want: "fatal: unable to access 'https://[REDACTED]@example.com/repo.git/': failure"},
		{event: "escaped_key_secret", message: `{"pass\u0077ord":"safe-prefix\"LEAKED-SECRET","status":"retrying"}`, want: `{"pass\u0077ord":"[REDACTED]","status":"retrying"}`},
		{event: "escaped_key_plain_secret", message: "\"pass\\u0077ord\": correct horse LEAKED-SECRET\nstatus: retrying", want: "\"pass\\u0077ord\": [REDACTED]\nstatus: retrying"},
		{event: "namespaced_plain_secret", message: "\"telegram.bot_token\": token-prefix-LEAKED-SECRET\nstatus: retrying", want: "\"telegram.bot_token\": [REDACTED]\nstatus: retrying"},
		{event: "single_key_plain_secret", message: "'client_secret': safe-prefix''LEAKED-SECRET\nstatus: retrying", want: "'client_secret': [REDACTED]\nstatus: retrying"},
		{event: "yaml_block_secret", message: "\"pass\\u0077ord\": |\n  safe-prefix LEAKED-SECRET\nstatus: retrying", want: "\"pass\\u0077ord\": [REDACTED]\nstatus: retrying"},
		{event: "multiline_quoted_secret", message: "password: 'safe-prefix\n  LEAKED-SECRET'\nstatus: retrying", want: "password: '[REDACTED]'\nstatus: retrying"},
		{event: "multiline_blank_secret", message: "password: 'safe-prefix\n\n  LEAKED-SECRET'\nstatus: retrying", want: "password: '[REDACTED]'\nstatus: retrying"},
		{event: "nested_block_secret", message: "config:\n  password: |\n    LEAKED-SECRET\n  status: retrying\nnext: ok", want: "config:\n  password: [REDACTED]\n  status: retrying\nnext: ok"},
		{event: "tagged_block_secret", message: "password: !!str |\n  safe-prefix\n \n  LEAKED-SECRET\nstatus: retrying", want: "password: [REDACTED]\nstatus: retrying"},
	}
	for _, test := range cases {
		runtime.LogEvent("error", "fixture", test.event, test.message)
	}
	assertRedactedEvent := func(entries []LogEntry, event, want string) {
		t.Helper()
		for _, entry := range entries {
			if entry.Event == event {
				if entry.Text != want || strings.Contains(entry.Text, "LEAKED-SECRET") || strings.Contains(entry.Text, "DOUBLED-SECRET") || strings.Contains(entry.Text, "SECOND-WA-SECRET") || strings.Contains(entry.Text, "LEAKED-ACTUAL-WA-PRIVATE") || strings.Contains(entry.Text, "LEAKED-FMT-WA-PRIVATE") || strings.Contains(entry.Text, "LEAKED-URL-PASSWORD") {
					t.Fatalf("retained quoted credential = %q", entry.Text)
				}
				return
			}
		}
		t.Fatalf("quoted credential event %q was not retained", event)
	}
	for _, test := range cases {
		assertRedactedEvent(runtime.Logs(), test.event, test.want)
	}
	runtime.Close()

	paths, err := filepath.Glob(filepath.Join(directory, "runtime-*.jsonl"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("persistent log paths = %v, %v", paths, err)
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "LEAKED-SECRET") || strings.Contains(string(data), "DOUBLED-SECRET") || strings.Contains(string(data), "SECOND-WA-SECRET") || strings.Contains(string(data), "LEAKED-ACTUAL-WA-PRIVATE") || strings.Contains(string(data), "LEAKED-FMT-WA-PRIVATE") || strings.Contains(string(data), "LEAKED-URL-PASSWORD") {
			t.Fatalf("quoted credential persisted in %s", filepath.Base(path))
		}
	}

	restored := NewRuntimeAt(directory, "quoted-escape-restore")
	defer restored.Close()
	for _, test := range cases {
		assertRedactedEvent(restored.Logs(), test.event, test.want)
	}
}

func TestRuntimeLogRestoresWorstCaseEscapedEntry(t *testing.T) {
	directory := t.TempDir()
	message := strings.Repeat("<", maxLogEntryRunes-1)
	first := NewRuntimeAt(directory, "escaped")
	first.LogEvent("error", "fixture", "escaped_entry", message)
	first.Close()

	second := NewRuntimeAt(directory, "escaped-restore")
	defer second.Close()
	for _, entry := range second.Logs() {
		if entry.Event == "escaped_entry" {
			if entry.Text != message {
				t.Fatalf("restored escaped entry length = %d, want %d", len([]rune(entry.Text)), len([]rune(message)))
			}
			return
		}
	}
	t.Fatalf("worst-case JSON-escaped entry was not restored: %#v", second.Logs())
}

func TestRuntimeLogNormalizesUnsafeRetainedEntry(t *testing.T) {
	directory := t.TempDir()
	entry := LogEntry{
		At: time.Now().UTC(), Level: "**ERROR**", Component: "[bad](target)", Event: "event`name",
		Instance: "instance]", Text: "authorization: Bearer retained-secret\x1b[31m",
	}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "runtime-unsafe.jsonl")
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	runtime := NewRuntimeAt(directory, "restore-normalized")
	defer runtime.Close()
	logs := runtime.Logs()
	if len(logs) < 2 {
		t.Fatalf("restored logs = %#v", logs)
	}
	got := logs[0]
	if strings.Contains(got.Text, "retained-secret") || got.Text != "authorization: [REDACTED]" {
		t.Fatalf("restored text was not sanitized and redacted: %q", got.Text)
	}
	if got.Level != "error" || got.Component != "badtarget" || got.Event != "eventname" || got.Instance != "instance" {
		t.Fatalf("restored metadata was not normalized: %#v", got)
	}
}

func TestRuntimeLogRestoreMergesOverlappingSessionsChronologically(t *testing.T) {
	directory := t.TempDir()
	base := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)
	writeEntries := func(name string, entries ...LogEntry) {
		t.Helper()
		var data []byte
		for _, entry := range entries {
			line, err := json.Marshal(entry)
			if err != nil {
				t.Fatal(err)
			}
			data = append(data, line...)
			data = append(data, '\n')
		}
		if err := os.WriteFile(filepath.Join(directory, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeEntries("runtime-20260808T000000.000000000Z-first-01.jsonl",
		LogEntry{At: base, Event: "first_early", Text: "first early"},
		LogEntry{At: base.Add(2 * time.Second), Event: "first_tie_a", Text: "first tie a"},
		LogEntry{At: base.Add(2 * time.Second), Event: "first_tie_b", Text: "first tie b"},
		LogEntry{At: base.Add(3 * time.Second), Event: "first_late", Text: "first late"},
	)
	writeEntries("runtime-20260808T000001.000000000Z-second-01.jsonl",
		LogEntry{At: base.Add(time.Second), Event: "second_early", Text: "second early"},
		LogEntry{At: base.Add(2 * time.Second), Event: "second_tie", Text: "second tie"},
	)

	persistence := newRuntimeLogPersistence(directory, "restore-order")
	defer persistence.close()
	entries, err := persistence.restore()
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(entries))
	for _, entry := range entries {
		got = append(got, entry.Event)
	}
	want := []string{"first_early", "second_early", "first_tie_a", "first_tie_b", "second_tie", "first_late"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("restored event order = %v, want %v", got, want)
	}
}

func TestRuntimeLogRestoreDirectoryLockWaitIsBounded(t *testing.T) {
	directory := t.TempDir()
	lockFile, err := os.OpenFile(filepath.Join(directory, "runtime.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := lockRuntimeFile(lockFile); err != nil {
		_ = lockFile.Close()
		t.Fatal(err)
	}
	defer func() {
		unlockRuntimeFile(lockFile)
		_ = lockFile.Close()
	}()

	persistence := newRuntimeLogPersistence(directory, "contended-restore")
	persistence.wait = 25 * time.Millisecond
	defer persistence.close()
	started := time.Now()
	_, err = persistence.restore()
	elapsed := time.Since(started)
	if err == nil || !strings.Contains(err.Error(), "directory lock timed out") {
		t.Fatalf("restore error = %v, want bounded directory-lock timeout", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("contended restore blocked for %s", elapsed)
	}
}

func TestRuntimeLogConcurrentWritesAreBoundedAndNotDuplicated(t *testing.T) {
	runtime := NewRuntimeAt(t.TempDir(), "concurrent")
	defer runtime.Close()
	const workers, each = 12, 40
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			writer := runtime.Writer(fmt.Sprintf("worker-%d", worker))
			for index := 0; index < each; index++ {
				_, _ = fmt.Fprintf(writer, "%d-%d\n", worker, index)
			}
		}(worker)
	}
	group.Wait()
	seen := map[string]bool{}
	for _, entry := range runtime.Logs() {
		if entry.Event != "output" || !strings.HasPrefix(entry.Component, "worker-") {
			continue
		}
		if seen[entry.Component+"/"+entry.Text] {
			t.Fatalf("duplicate captured record %s/%s", entry.Component, entry.Text)
		}
		seen[entry.Component+"/"+entry.Text] = true
	}
	if len(seen) != workers*each {
		t.Fatalf("captured %d concurrent records, want %d", len(seen), workers*each)
	}
}

func TestRuntimeLogRotationAndClear(t *testing.T) {
	directory := t.TempDir()
	runtime := NewRuntimeAt(directory, "rotation")
	defer runtime.Close()
	payload := strings.Repeat("x", maxLogEntryRunes)
	for index := 0; index < 1100; index++ {
		runtime.LogEvent("info", "rotation", "line", fmt.Sprintf("%04d-%s", index, payload))
	}
	if err := runtime.persist.control("flush"); err != nil {
		t.Fatal(err)
	}
	paths, _ := filepath.Glob(filepath.Join(directory, "runtime-*.jsonl"))
	if len(paths) < 2 || len(paths) > maxRuntimeLogFiles {
		t.Fatalf("rotated paths = %d, want 2..%d", len(paths), maxRuntimeLogFiles)
	}
	count, err := runtime.ClearLogsResult()
	if err != nil {
		t.Fatal(err)
	}
	if count != 1101 {
		t.Fatalf("clear count = %d, want 1101", count)
	}
	paths, _ = filepath.Glob(filepath.Join(directory, "runtime-*.jsonl"))
	if len(paths) != 0 || len(runtime.Logs()) != 0 {
		t.Fatalf("clear left files=%v entries=%d", paths, len(runtime.Logs()))
	}
}

func TestRuntimeRecoverPanicRecordsStackAndPreservesPanic(t *testing.T) {
	runtime := NewRuntimeAt(t.TempDir(), "panic")
	defer runtime.Close()
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		func() {
			defer runtime.RecoverPanic("orchestrator", "worker_panic")
			panic("fixture failure")
		}()
	}()
	if recovered != "fixture failure" {
		t.Fatalf("recovered panic = %#v", recovered)
	}
	logs := runtime.Logs()
	last := logs[len(logs)-1]
	if last.Level != "fatal" || last.Event != "worker_panic" || !strings.Contains(last.Text, "fixture failure") || !strings.Contains(last.Text, "goroutine") {
		t.Fatalf("panic evidence = %#v", last)
	}
}

func TestRuntimeLogPersistenceFailureFallsBackToMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(path, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := NewRuntimeAt(path, "fallback")
	defer runtime.Close()
	runtime.LogEvent("error", "storage", "fixture", "still visible")
	logs := runtime.Logs()
	var applicationEvent, restoreFailure, appendFailure bool
	for _, entry := range logs {
		applicationEvent = applicationEvent || entry.Event == "fixture" && entry.Text == "still visible"
		restoreFailure = restoreFailure || entry.Component == "runtime_log" && entry.Event == "restore_failed"
		appendFailure = appendFailure || entry.Component == "runtime_log" && entry.Event == "append_failed"
	}
	if !applicationEvent || !restoreFailure || !appendFailure {
		t.Fatalf("in-memory fallback logs = %#v", logs)
	}
}

func TestRuntimeLogInformationalAppendFailureIsVisible(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(path, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := NewRuntimeAt(path, "async-fallback")
	defer runtime.Close()
	deadline := time.Now().Add(time.Second)
	for {
		for _, entry := range runtime.Logs() {
			if entry.Component == "runtime_log" && entry.Event == "append_failed" {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("informational persistence failure was not exposed: %#v", runtime.Logs())
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRuntimeLogPersistenceWaitIsBoundedAndVisible(t *testing.T) {
	runtime := NewRuntimeAt(t.TempDir(), "blocked")
	runtime.persist.wait = 20 * time.Millisecond
	runtime.persist.mu.Lock() // Simulate a stalled filesystem operation in the worker.
	started := time.Now()
	runtime.LogEvent("error", "fixture", "blocked_write", "important evidence")
	elapsed := time.Since(started)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("important log write blocked for %s", elapsed)
	}
	var found bool
	for _, entry := range runtime.Logs() {
		found = found || entry.Component == "runtime_log" && entry.Event == "append_failed" && strings.Contains(entry.Text, "timed out")
	}
	if !found {
		t.Fatalf("persistence timeout was not exposed: %#v", runtime.Logs())
	}
	runtime.persist.mu.Unlock()
	runtime.Close()
}

func TestRuntimeClearIsOrderedAfterConcurrentAppend(t *testing.T) {
	directory := t.TempDir()
	runtime := NewRuntimeAt(directory, "ordered-clear")

	// Pause after the application record is accepted in memory but before its
	// durable append. Clear must wait behind that append rather than overtake it.
	reachedBoundary := make(chan struct{})
	releaseAppend := make(chan struct{})
	runtime.logEventHook = func() {
		close(reachedBoundary)
		<-releaseAppend
	}
	logged := make(chan struct{})
	go func() {
		runtime.LogEvent("error", "fixture", "before_clear", "must stay cleared")
		close(logged)
	}()
	<-reachedBoundary
	cleared := make(chan error, 1)
	go func() {
		_, err := runtime.ClearLogsResult()
		cleared <- err
	}()
	close(releaseAppend)
	<-logged
	if err := <-cleared; err != nil {
		t.Fatal(err)
	}
	runtime.logEventHook = nil
	runtime.Close()

	restored := NewRuntimeAt(directory, "ordered-restore")
	defer restored.Close()
	for _, entry := range restored.Logs() {
		if entry.Event == "before_clear" {
			t.Fatalf("record accepted before clear reappeared after restart: %#v", restored.Logs())
		}
	}
}

func TestRuntimeCloseFlushesUnterminatedComponentOutput(t *testing.T) {
	directory := t.TempDir()
	runtime := NewRuntimeAt(directory, "partial")
	_, _ = runtime.Write([]byte("process tail"))
	_, _ = runtime.Writer("harness").Write([]byte("harness tail"))
	runtime.Close()

	restored := NewRuntimeAt(directory, "restore")
	defer restored.Close()
	var processTail, harnessTail bool
	for _, entry := range restored.Logs() {
		processTail = processTail || entry.Event == "stderr_partial" && entry.Text == "process tail"
		harnessTail = harnessTail || entry.Event == "output_partial" && entry.Component == "harness" && entry.Text == "harness tail"
	}
	if !processTail || !harnessTail {
		t.Fatalf("unterminated output was not retained: %#v", restored.Logs())
	}
}

func TestRuntimeRetentionNeverDeletesAnotherActiveSession(t *testing.T) {
	directory := t.TempDir()
	active := NewRuntimeAt(directory, "active")
	active.LogEvent("error", "fixture", "before", "active before retention")
	for index := 0; index < maxRuntimeLogFiles+3; index++ {
		short := NewRuntimeAt(directory, fmt.Sprintf("short-%d", index))
		short.Close()
	}
	active.LogEvent("error", "fixture", "after", "active after retention")
	active.Close()

	restored := NewRuntimeAt(directory, "reader")
	defer restored.Close()
	var before, after bool
	for _, entry := range restored.Logs() {
		before = before || entry.Event == "before"
		after = after || entry.Event == "after"
	}
	if !before || !after {
		t.Fatalf("active session was unlinked during retention: %#v", restored.Logs())
	}
	paths, _ := filepath.Glob(filepath.Join(directory, "runtime-*.jsonl"))
	if len(paths) > maxRuntimeLogFiles {
		t.Fatalf("retention has %d files, want at most %d", len(paths), maxRuntimeLogFiles)
	}
}

func TestRuntimeClearReportsAnotherActiveSession(t *testing.T) {
	directory := t.TempDir()
	active := NewRuntimeAt(directory, "active-clear")
	defer active.Close()
	if err := active.persist.control("flush"); err != nil {
		t.Fatal(err)
	}
	other := NewRuntimeAt(directory, "clearer")
	defer other.Close()
	if _, err := other.ClearLogsResult(); err == nil || !strings.Contains(err.Error(), "active runtime log") {
		t.Fatalf("clear error = %v, want active-session evidence", err)
	}
}
