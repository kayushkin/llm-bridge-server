package possessionstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// PhotoStoreOverHTTP is PhotoStoreReads over a photo-store that runs apart, at
// URL: what a check against a standalone photo-store reads. In the server,
// photo-store runs in the same process and is read in Go.
type PhotoStoreOverHTTP struct {
	URL        string
	HTTPClient *http.Client
}

var _ PhotoStoreReads = PhotoStoreOverHTTP{}

func (store PhotoStoreOverHTTP) get(ctx context.Context, path string, into any) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, store.URL+path, nil)
	if err != nil {
		return nil, err
	}
	response, err := store.HTTPClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("photo-store GET %s: %w", path, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("photo-store GET %s: %d %s", path, response.StatusCode, bytes.TrimSpace(body))
	}
	if into != nil {
		if err := json.Unmarshal(body, into); err != nil {
			return nil, fmt.Errorf("photo-store GET %s: %w", path, err)
		}
	}
	return body, nil
}

func (store PhotoStoreOverHTTP) Photo(ctx context.Context, photoID string) (PhotoFacts, error) {
	var photo PhotoFacts
	_, err := store.get(ctx, "/photos/"+url.PathEscape(photoID), &photo)
	return photo, err
}

func (store PhotoStoreOverHTTP) Thumbnail(ctx context.Context, photoID string) ([]byte, error) {
	return store.get(ctx, "/photos/"+url.PathEscape(photoID)+"/thumbnail", nil)
}

func (store PhotoStoreOverHTTP) ConfirmedFacesOfPerson(ctx context.Context, photoID, personID string) ([]string, error) {
	var links []struct {
		FaceID *string `json:"face_id"`
	}
	if _, err := store.get(ctx, "/photo-people?photo_id="+url.QueryEscape(photoID)+"&person_id="+url.QueryEscape(personID)+"&status=confirmed", &links); err != nil {
		return nil, err
	}
	faceIDs := make([]string, 0, len(links))
	for _, link := range links {
		if link.FaceID != nil {
			faceIDs = append(faceIDs, *link.FaceID)
		}
	}
	return faceIDs, nil
}

func (store PhotoStoreOverHTTP) Faces(ctx context.Context, photoID string) ([]PhotoFace, error) {
	var faces []PhotoFace
	_, err := store.get(ctx, "/photos/"+url.PathEscape(photoID)+"/faces", &faces)
	return faces, err
}
