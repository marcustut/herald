package client

import "testing"

func TestHTTPErrorWithoutCode(t *testing.T) {
	err := &HTTPError{StatusCode: 500}
	if err.Error() != "gateway returned 500" {
		t.Fatalf("message = %q", err.Error())
	}
}
