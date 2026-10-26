package config

import "testing"

func TestParseByteSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{in: "1024", want: 1024},
		{in: "512B", want: 512},
		{in: "500MB", want: 500_000_000},
		{in: "10GiB", want: 10 << 30},
		{in: "1.5 TB", want: 1_500_000_000_000},
		{in: " 2kib ", want: 2048},
		{in: "50gb", want: 50_000_000_000},
		{in: "", wantErr: true},
		{in: "0", wantErr: true},
		{in: "-5MB", wantErr: true},
		{in: "10 parsecs", wantErr: true},
		{in: "GiB", wantErr: true},
		{in: "99999999TiB", wantErr: true},
	}

	for _, tt := range tests {
		got, err := ParseByteSize(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseByteSize(%q) = %d, want an error", tt.in, got)
			}

			continue
		}

		if err != nil || got != tt.want {
			t.Errorf("ParseByteSize(%q) = (%d, %v), want (%d, nil)", tt.in, got, err, tt.want)
		}
	}
}

func TestResolveReceiverSizeLimits(t *testing.T) {
	t.Parallel()

	base := FileReceiver{ID: "a", AllowedServers: []string{testSenderID}, Path: "/mnt/a"}

	got, err := ResolveReceiver(base, nil, nil, "")
	if err != nil || got.MaxObjectSize != DefaultReceiverMaxObjectSize || got.Quota != 0 {
		t.Errorf("unset limits: (MaxObjectSize %d, Quota %d, err %v), want (%d, 0, nil)", got.MaxObjectSize, got.Quota, err, DefaultReceiverMaxObjectSize)
	}

	set := base
	set.MaxObjectSize, set.Quota = "1GiB", "1TB"

	got, err = ResolveReceiver(set, nil, nil, "")
	if err != nil || got.MaxObjectSize != 1<<30 || got.Quota != 1_000_000_000_000 {
		t.Errorf("set limits: (MaxObjectSize %d, Quota %d, err %v), want (%d, %d, nil)", got.MaxObjectSize, got.Quota, err, 1<<30, 1_000_000_000_000)
	}

	for _, bad := range []FileReceiver{{MaxObjectSize: "big"}, {Quota: "-1GB"}} {
		fr := base
		fr.MaxObjectSize, fr.Quota = bad.MaxObjectSize, bad.Quota

		if _, err := ResolveReceiver(fr, nil, nil, ""); err == nil {
			t.Errorf("ResolveReceiver(max-object-size %q, quota %q) = nil error, want one", fr.MaxObjectSize, fr.Quota)
		}
	}
}
