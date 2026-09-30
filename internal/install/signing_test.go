package install

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

func TestParseIdentities(t *testing.T) {
	out := `  1) 34917FE73F26063F1768E7CA4F1B17052B8F1C94 "Apple Development: Alexander Finadeev (98L5HG8P5X)"
  2) AD7EDDB95D5BC677EF933173707D70A468F29C94 "Apple Distribution: Acme (T3B5G56RNZ)"
     2 valid identities found`
	ids := parseIdentities(out)
	if len(ids) != 2 || ids[0].Hash != "34917FE73F26063F1768E7CA4F1B17052B8F1C94" ||
		ids[1].Name != "Apple Distribution: Acme (T3B5G56RNZ)" {
		t.Fatalf("got %+v", ids)
	}
}

// profilePlist mimics `security cms -D` output for a team wildcard profile.
func profilePlist(cert []byte, expires string, udids ...string) []byte {
	var devs strings.Builder
	for _, u := range udids {
		devs.WriteString("<string>" + u + "</string>")
	}
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Name</key><string>iOS Team Provisioning Profile: *</string>
  <key>TeamIdentifier</key><array><string>T3B5G56RNZ</string></array>
  <key>ExpirationDate</key><date>` + expires + `</date>
  <key>DeveloperCertificates</key><array><data>
    ` + base64.StdEncoding.EncodeToString(cert) + `
  </data></array>
  <key>Entitlements</key><dict>
    <key>application-identifier</key><string>T3B5G56RNZ.*</string>
    <key>get-task-allow</key><true/>
    <key>keychain-access-groups</key><array><string>T3B5G56RNZ.*</string></array>
  </dict>
  <key>ProvisionedDevices</key><array>` + devs.String() + `</array>
</dict></plist>`)
}

func TestProfileSelection(t *testing.T) {
	cert := []byte("fake DER certificate")
	sum := sha1.Sum(cert)
	hash := strings.ToUpper(hex.EncodeToString(sum[:]))
	now := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)

	good, err := parseProfile(profilePlist(cert, "2027-09-04T08:29:20Z", "00008110-000478C13C11801E", "6499997edfc5ede67fb72ac168906b4249c07ab4"))
	if err != nil {
		t.Fatal(err)
	}
	if good.appID != "T3B5G56RNZ.*" || good.team != "T3B5G56RNZ" || !good.certs[hash] || good.expires.Year() != 2027 {
		t.Fatalf("parsed %+v", good)
	}
	if !strings.Contains(string(good.entitlements), "get-task-allow") {
		t.Fatalf("entitlements: %s", good.entitlements)
	}
	old, _ := parseProfile(profilePlist(cert, "2025-01-01T00:00:00Z", "6499997EDFC5EDE67FB72AC168906B4249C07AB4"))
	other, _ := parseProfile(profilePlist([]byte("someone else"), "2027-01-01T00:00:00Z", "6499997edfc5ede67fb72ac168906b4249c07ab4"))
	all := []profile{old, other, good}

	// UDID match is case-insensitive; the expired and foreign-cert ones drop out
	got, err := pickProfiles(all, nil, hash, "6499997EDFC5EDE67FB72AC168906B4249C07AB4", now)
	if err != nil || len(got) != 1 || !got[0].expires.Equal(good.expires) {
		t.Fatalf("pick: %+v, %v", got, err)
	}
	if p, ok := matchProfile(got, "ru.reforce.platform"); !ok || p.appID != "T3B5G56RNZ.*" {
		t.Fatal("wildcard did not match an arbitrary bundle id")
	}

	// a phone not in the team: the error says which filter left nothing
	_, err = pickProfiles(all, nil, hash, "00008030-DEADBEEF", now)
	if err == nil || !strings.Contains(err.Error(), "1 expired, 1 issued for another certificate, 1 without this device") {
		t.Fatalf("err: %v", err)
	}
}

func TestConcreteEntitlements(t *testing.T) {
	in := []byte(`<dict><key>application-identifier</key><string>T3B5G56RNZ.*</string>` +
		`<key>keychain-access-groups</key><array><string>T3B5G56RNZ.*</string><string>com.apple.token</string></array>` +
		`<key>get-task-allow</key><true/></dict>`)
	got := string(concreteEntitlements(in, "dev.poligon.itestDemo"))
	want := `<dict><key>application-identifier</key><string>T3B5G56RNZ.dev.poligon.itestDemo</string>` +
		`<key>keychain-access-groups</key><array><string>T3B5G56RNZ.dev.poligon.itestDemo</string><string>com.apple.token</string></array>` +
		`<key>get-task-allow</key><true/></dict>`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}
