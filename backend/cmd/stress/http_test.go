package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

// getAll follows X-Next-Cursor to the last page, keeping the caller's query, with the page size
// asked for — or none, leaving the server's default (a listing whose maximum is lower, e.g.
// /customers, would refuse 200).
func TestGetAllFollowsCursors(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		limit      int
		want       []string
	}{
		{"listing maximum", "/api/v1/vehicles?q=x", maxListLimit, []string{"q=x&limit=200", "q=x&limit=200&cursor=p2", "q=x&limit=200&cursor=p3"}},
		{"own page size", "/api/v1/customers?q=x", 100, []string{"q=x&limit=100", "q=x&limit=100&cursor=p2", "q=x&limit=100&cursor=p3"}},
		{"server default", "/api/v1/customers?q=x", 0, []string{"q=x", "q=x&cursor=p2", "q=x&cursor=p3"}},
		{"server default, no query", "/api/v1/dealerships", 0, []string{"", "cursor=p2", "cursor=p3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seen []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = append(seen, r.URL.RawQuery)
				switch r.URL.Query().Get("cursor") {
				case "":
					w.Header().Set("X-Next-Cursor", "p2")
					fmt.Fprint(w, `[1,2]`)
				case "p2":
					w.Header().Set("X-Next-Cursor", "p3")
					fmt.Fprint(w, `[3,4]`)
				default:
					fmt.Fprint(w, `[5]`)
				}
			}))
			defer srv.Close()
			got, err := getAll[int](t.Context(), newAPI(srv.URL), tc.path, tc.limit, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, []int{1, 2, 3, 4, 5}) {
				t.Fatalf("items = %v", got)
			}
			if !slices.Equal(seen, tc.want) {
				t.Fatalf("queries = %v, want %v", seen, tc.want)
			}
		})
	}
}
