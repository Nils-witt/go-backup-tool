package backup

import (
	"reflect"
	"testing"
)

func TestPermissionCanViewCanDownload(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		perm         Permission
		wantView     bool
		wantDownload bool
		wantAdmin    bool
	}{
		{"none", 0, false, false, false},
		{"view only", PermissionView, true, false, false},
		{"download only implies view", PermissionDownload, true, true, false},
		{"both", PermissionView | PermissionDownload, true, true, false},
		{"admin only implies view and download", PermissionAdmin, true, true, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.perm.CanView(); got != tt.wantView {
				t.Errorf("CanView() = %v, want %v", got, tt.wantView)
			}

			if got := tt.perm.CanDownload(); got != tt.wantDownload {
				t.Errorf("CanDownload() = %v, want %v", got, tt.wantDownload)
			}

			if got := tt.perm.CanAdmin(); got != tt.wantAdmin {
				t.Errorf("CanAdmin() = %v, want %v", got, tt.wantAdmin)
			}
		})
	}
}

func TestPermissionCanViewLoginLogCanViewDownloadLog(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		perm            Permission
		wantLoginLog    bool
		wantDownloadLog bool
	}{
		{"none", 0, false, false},
		{"view alone does not grant either log", PermissionView, false, false},
		{"download alone does not grant either log", PermissionDownload, false, false},
		{"login log only", PermissionViewLoginLog, true, false},
		{"download log only", PermissionViewDownloadLog, false, true},
		{"both logs", PermissionViewLoginLog | PermissionViewDownloadLog, true, true},
		{"admin implies both logs", PermissionAdmin, true, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.perm.CanViewLoginLog(); got != tt.wantLoginLog {
				t.Errorf("CanViewLoginLog() = %v, want %v", got, tt.wantLoginLog)
			}

			if got := tt.perm.CanViewDownloadLog(); got != tt.wantDownloadLog {
				t.Errorf("CanViewDownloadLog() = %v, want %v", got, tt.wantDownloadLog)
			}
		})
	}
}

func TestPermissionNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		perm Permission
		want []string
	}{
		{0, nil},
		{PermissionView, []string{"view"}},
		{PermissionDownload, []string{"download"}},
		{PermissionView | PermissionDownload, []string{"view", "download"}},
		{PermissionAdmin, []string{"admin"}},
		{PermissionView | PermissionDownload | PermissionAdmin, []string{"view", "download", "admin"}},
		{PermissionViewLoginLog, []string{"login-log"}},
		{PermissionViewDownloadLog, []string{"download-log"}},
		{PermissionViewLoginLog | PermissionViewDownloadLog, []string{"login-log", "download-log"}},
	}

	for _, tt := range tests {
		if got := tt.perm.Names(); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("Permission(%d).Names() = %v, want %v", tt.perm, got, tt.want)
		}
	}
}

func TestParsePermissionsRoundTripsNames(t *testing.T) {
	t.Parallel()

	perm, err := ParsePermissions([]string{"view", "download", "admin", "login-log", "download-log"})
	if err != nil {
		t.Fatalf("ParsePermissions() unexpected error: %v", err)
	}

	if want := PermissionView | PermissionDownload | PermissionAdmin | PermissionViewLoginLog | PermissionViewDownloadLog; perm != want {
		t.Errorf("ParsePermissions() = %v, want %v", perm, want)
	}
}

func TestParsePermissionsEmpty(t *testing.T) {
	t.Parallel()

	perm, err := ParsePermissions(nil)
	if err != nil {
		t.Fatalf("ParsePermissions(nil) unexpected error: %v", err)
	}

	if perm != 0 {
		t.Errorf("ParsePermissions(nil) = %v, want 0", perm)
	}
}

func TestParsePermissionsRejectsUnknown(t *testing.T) {
	t.Parallel()

	if _, err := ParsePermissions([]string{"view", "delete"}); err == nil {
		t.Fatal("ParsePermissions() with an unknown name = nil error, want one")
	}
}
