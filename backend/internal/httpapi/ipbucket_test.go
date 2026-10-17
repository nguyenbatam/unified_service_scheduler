package httpapi

import "testing"

func TestIPBucketKey(t *testing.T) {
	tests := []struct {
		name, ip, want string
	}{
		{name: "IPv4 unchanged", ip: "203.0.113.7", want: "203.0.113.7"},
		{name: "IPv4-mapped IPv6 is IPv4", ip: "::ffff:203.0.113.7", want: "203.0.113.7"},
		{name: "IPv6 truncated to /64", ip: "2001:db8:1:2:aaaa:bbbb:cccc:dddd", want: "2001:db8:1:2::/64"},
		{name: "another host in the same /64 shares it", ip: "2001:db8:1:2::1", want: "2001:db8:1:2::/64"},
		{name: "the next /64 is a different bucket", ip: "2001:db8:1:3::1", want: "2001:db8:1:3::/64"},
		{name: "zone is dropped", ip: "fe80::1%eth0", want: "fe80::/64"},
		{name: "unparsable kept as is", ip: "not-an-ip", want: "not-an-ip"},
		{name: "empty kept as is", ip: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ipBucketKey(tt.ip); got != tt.want {
				t.Fatalf("ipBucketKey(%q) = %q, want %q", tt.ip, got, tt.want)
			}
		})
	}
}
