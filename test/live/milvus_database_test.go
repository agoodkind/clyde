//go:build live

package live

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"testing"
	"time"
)

const (
	// liveMilvusAddress is the production Milvus server. A live test creates
	// its own database there with a unique name, reads and writes only that
	// database, and drops it in cleanup.
	liveMilvusAddress = "localhost:19530"
	// liveMilvusDatabasePrefix starts the name of every database a live test
	// creates. The rest of the name is 32 random hex characters.
	liveMilvusDatabasePrefix = "clyde_live_"
	milvusRESTTimeout        = 30 * time.Second
)

// milvusDatabaseRequest is the body of the Milvus REST database calls.
type milvusDatabaseRequest struct {
	DatabaseName string `json:"dbName,omitempty"`
}

// milvusCollectionRequest is the body of the Milvus REST collection drop call.
type milvusCollectionRequest struct {
	DatabaseName   string `json:"dbName,omitempty"`
	CollectionName string `json:"collectionName"`
}

// milvusStatusResponse is the part of every Milvus REST response that reports
// success. Code 0 is success.
type milvusStatusResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// milvusNamesResponse is a Milvus REST list response.
type milvusNamesResponse struct {
	Code    int      `json:"code"`
	Message string   `json:"message"`
	Data    []string `json:"data"`
}

// createLiveMilvusDatabase creates a database named clyde_live_ followed by
// 32 random hex characters on the Milvus server at liveMilvusAddress. It fails
// when the name exists. Cleanup drops every collection in the database, drops the database,
// and fails when the database remains.
func createLiveMilvusDatabase(t *testing.T) string {
	t.Helper()

	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		t.Fatalf("generate Milvus database name: %v", err)
	}
	name := liveMilvusDatabasePrefix + hex.EncodeToString(random)
	existing, err := listMilvusDatabases()
	if err != nil {
		t.Fatalf("list Milvus databases at %s: %v", liveMilvusAddress, err)
	}
	if slices.Contains(existing, name) {
		t.Fatalf("Milvus database %s already exists at %s", name, liveMilvusAddress)
	}
	var created milvusStatusResponse
	if err := callMilvusREST("/v2/vectordb/databases/create", milvusDatabaseRequest{DatabaseName: name}, &created); err != nil {
		t.Fatalf("create Milvus database %s: %v", name, err)
	}
	if created.Code != 0 {
		t.Fatalf("create Milvus database %s: code %d: %s", name, created.Code, created.Message)
	}
	t.Logf("created Milvus database %s at %s on %s", name, time.Now().UTC().Format(time.RFC3339), liveMilvusAddress)
	t.Cleanup(func() {
		dropLiveMilvusDatabase(t, name)
	})
	return name
}

// dropLiveMilvusDatabase drops every collection in the named database and then
// the database. It reports each failure and a database that remains.
func dropLiveMilvusDatabase(t *testing.T, name string) {
	t.Helper()

	var collections milvusNamesResponse
	if err := callMilvusREST("/v2/vectordb/collections/list", milvusDatabaseRequest{DatabaseName: name}, &collections); err != nil {
		t.Errorf("list collections of Milvus database %s: %v", name, err)
	}
	for _, collection := range collections.Data {
		var dropped milvusStatusResponse
		request := milvusCollectionRequest{DatabaseName: name, CollectionName: collection}
		if err := callMilvusREST("/v2/vectordb/collections/drop", request, &dropped); err != nil {
			t.Errorf("drop collection %s of Milvus database %s: %v", collection, name, err)
			continue
		}
		if dropped.Code != 0 {
			t.Errorf("drop collection %s of Milvus database %s: code %d: %s", collection, name, dropped.Code, dropped.Message)
		}
	}
	var dropped milvusStatusResponse
	if err := callMilvusREST("/v2/vectordb/databases/drop", milvusDatabaseRequest{DatabaseName: name}, &dropped); err != nil {
		t.Errorf("drop Milvus database %s: %v", name, err)
	}
	if dropped.Code != 0 {
		t.Errorf("drop Milvus database %s: code %d: %s", name, dropped.Code, dropped.Message)
	}
	remaining, err := listMilvusDatabases()
	if err != nil {
		t.Errorf("list Milvus databases after dropping %s: %v", name, err)
		return
	}
	if slices.Contains(remaining, name) {
		t.Errorf("Milvus database %s remains after drop", name)
		return
	}
	t.Logf("dropped Milvus database %s with collections %v at %s", name, collections.Data, time.Now().UTC().Format(time.RFC3339))
}

func listMilvusDatabases() ([]string, error) {
	var listed milvusNamesResponse
	if err := callMilvusREST("/v2/vectordb/databases/list", milvusDatabaseRequest{DatabaseName: ""}, &listed); err != nil {
		return nil, err
	}
	if listed.Code != 0 {
		return nil, fmt.Errorf("list databases: code %d: %s", listed.Code, listed.Message)
	}
	return listed.Data, nil
}

// callMilvusREST posts one JSON request to the Milvus REST API at
// liveMilvusAddress and decodes the JSON response into response.
func callMilvusREST[Request milvusDatabaseRequest | milvusCollectionRequest, Response milvusStatusResponse | milvusNamesResponse](
	path string,
	request Request,
	response *Response,
) error {
	body, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encode Milvus request for %s: %w", path, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), milvusRESTTimeout)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+liveMilvusAddress+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build Milvus request for %s: %w", path, err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpResponse, err := http.DefaultClient.Do(httpRequest)
	if err != nil {
		return fmt.Errorf("post Milvus request to %s: %w", path, err)
	}
	defer func() { _ = httpResponse.Body.Close() }()
	responseBody, err := io.ReadAll(httpResponse.Body)
	if err != nil {
		return fmt.Errorf("read Milvus response from %s: %w", path, err)
	}
	if httpResponse.StatusCode != http.StatusOK {
		return fmt.Errorf("milvus REST call %s returned HTTP %d", path, httpResponse.StatusCode)
	}
	if err := json.Unmarshal(responseBody, response); err != nil {
		return fmt.Errorf("decode Milvus response from %s: %w", path, err)
	}
	return nil
}
