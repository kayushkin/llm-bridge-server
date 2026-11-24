package peoplestore

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrPrincipalStoreUnavailable means principal-store could not say whether a
// principal exists. The write is refused with a 502 rather than stored
// unchecked.
var ErrPrincipalStoreUnavailable = errors.New("principal-store could not answer")

// PrincipalChecker says whether principal-store holds a principal.
type PrincipalChecker interface {
	// CheckPrincipal returns nil when the principal exists, an error wrapping
	// ErrInvalid when principal-store says it does not, and one wrapping
	// ErrPrincipalStoreUnavailable when it cannot tell.
	CheckPrincipal(id string) error
}

// HTTPPrincipalChecker asks principal-store's GET /principals/{id}.
type HTTPPrincipalChecker struct {
	BaseURL string
	Client  *http.Client
}

// NewHTTPPrincipalChecker checks principals against the principal-store at
// baseURL.
func NewHTTPPrincipalChecker(baseURL string) *HTTPPrincipalChecker {
	return &HTTPPrincipalChecker{BaseURL: strings.TrimRight(baseURL, "/"), Client: &http.Client{Timeout: 5 * time.Second}}
}

func (checker *HTTPPrincipalChecker) CheckPrincipal(id string) error {
	response, err := checker.Client.Get(checker.BaseURL + "/principals/" + url.PathEscape(id))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPrincipalStoreUnavailable, err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	switch response.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("%w: principal-store holds no principal %s", ErrInvalid, id)
	default:
		return fmt.Errorf("%w: GET /principals/%s answered %d: %s", ErrPrincipalStoreUnavailable, id, response.StatusCode, strings.TrimSpace(string(body)))
	}
}

// checkPrincipalID validates the shape of a principal id and asks
// principal-store whether it exists. "" means none and is not checked.
func (s *Store) checkPrincipalID(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", nil
	}
	if !principalIDPattern.MatchString(id) {
		return "", fmt.Errorf("%w: principal_id %q is not a principal-store id (principal_000001)", ErrInvalid, id)
	}
	if err := s.principals.CheckPrincipal(id); err != nil {
		return "", err
	}
	return id, nil
}
