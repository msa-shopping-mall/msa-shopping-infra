package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	elastic "github.com/elastic/go-elasticsearch/v8"
	"github.com/elastic/go-elasticsearch/v8/esapi"
)

const (
	indexName     = "autocomplete"
	defaultESHost = "http://localhost:9200"
)

type upsertRequest struct {
	Keyword string                 `json:"keyword"`
	Weight  int                    `json:"weight,omitempty"`
	Meta    map[string]interface{} `json:"meta,omitempty"`
}

type suggestResponse struct {
	Suggestions []string `json:"suggestions"`
}

func main() {
	esURL := strings.TrimSpace(os.Getenv("ELASTICSEARCH_URL"))
	if esURL == "" {
		esURL = defaultESHost
	}

	es, err := elastic.NewClient(elastic.Config{
		Addresses: []string{esURL},
		Username:  os.Getenv("ELASTICSEARCH_USERNAME"),
		Password:  os.Getenv("ELASTICSEARCH_PASSWORD"),
	})
	if err != nil {
		log.Fatalf("elasticsearch 초기화 실패: %v", err)
	}

	ctx := context.Background()
	if err := ensureIndex(ctx, es); err != nil {
		log.Fatalf("인덱스 준비 실패: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/keywords", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST로 요청하세요", http.StatusMethodNotAllowed)
			return
		}
		var req upsertRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "잘못된 요청 본문", http.StatusBadRequest)
			return
		}
		if err := upsertKeyword(ctx, es, req); err != nil {
			log.Printf("upsert 실패: %v", err)
			http.Error(w, "업서트 실패", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("/suggest", func(w http.ResponseWriter, r *http.Request) {
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		if q == "" {
			http.Error(w, "q 파라미터가 필요합니다", http.StatusBadRequest)
			return
		}
		suggestions, err := suggest(ctx, es, q)
		if err != nil {
			log.Printf("suggest 실패: %v", err)
			http.Error(w, "검색 실패", http.StatusInternalServerError)
			return
		}
		writeJSON(w, suggestResponse{Suggestions: suggestions})
	})

	port := os.Getenv("PORT")
	if strings.TrimSpace(port) == "" {
		port = "8080"
	}
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 3 * time.Second,
	}
	log.Printf("autocomplete API 시작: 포트 %s, ES %s", port, esURL)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("서버 종료: %v", err)
	}
}

func ensureIndex(ctx context.Context, es *elastic.Client) error {
	res, err := es.Indices.Exists([]string{indexName}, es.Indices.Exists.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("인덱스 확인 실패: %w", err)
	}
	defer discard(res.Body)
	if res.StatusCode == http.StatusOK {
		return nil
	}
	if res.StatusCode != http.StatusNotFound {
		return fmt.Errorf("인덱스 확인 응답 코드: %d", res.StatusCode)
	}

	body := strings.NewReader(indexMapping)
	createRes, err := es.Indices.Create(indexName, es.Indices.Create.WithBody(body), es.Indices.Create.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("인덱스 생성 실패: %w", err)
	}
	defer discard(createRes.Body)
	if createRes.IsError() {
		return fmt.Errorf("인덱스 생성 응답 에러: %s", createRes.String())
	}
	return nil
}

func upsertKeyword(ctx context.Context, es *elastic.Client, req upsertRequest) error {
	keyword := strings.TrimSpace(req.Keyword)
	if keyword == "" {
		return errors.New("keyword가 비어 있음")
	}
	if req.Weight == 0 {
		req.Weight = 1
	}
	if req.Meta == nil {
		req.Meta = map[string]interface{}{}
	}

	doc := map[string]interface{}{
		"keyword": keyword,
		"suggest": map[string]interface{}{
			"input":  []string{keyword},
			"weight": req.Weight,
		},
		"meta": req.Meta,
	}
	payload := map[string]interface{}{
		"doc":           doc,
		"doc_as_upsert": true,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("payload 직렬화 실패: %w", err)
	}

	updateReq := esapi.UpdateRequest{
		Index:      indexName,
		DocumentID: docID(keyword),
		Body:       bytes.NewReader(body),
	}
	res, err := updateReq.Do(ctx, es)
	if err != nil {
		return fmt.Errorf("업서트 요청 실패: %w", err)
	}
	defer discard(res.Body)
	if res.IsError() {
		return fmt.Errorf("업서트 응답 에러: %s", res.String())
	}
	return nil
}

func suggest(ctx context.Context, es *elastic.Client, q string) ([]string, error) {
	query := map[string]interface{}{
		"suggest": map[string]interface{}{
			"ac": map[string]interface{}{
				"prefix": q,
				"completion": map[string]interface{}{
					"field":           "suggest",
					"skip_duplicates": true,
					"size":            10,
				},
			},
		},
	}
	body, err := json.Marshal(query)
	if err != nil {
		return nil, fmt.Errorf("쿼리 직렬화 실패: %w", err)
	}
	res, err := es.Search(
		es.Search.WithContext(ctx),
		es.Search.WithIndex(indexName),
		es.Search.WithBody(bytes.NewReader(body)),
	)
	if err != nil {
		return nil, fmt.Errorf("검색 요청 실패: %w", err)
	}
	defer discard(res.Body)
	if res.IsError() {
		return nil, fmt.Errorf("검색 응답 에러: %s", res.String())
	}

	var parsed struct {
		Suggest map[string][]struct {
			Options []struct {
				Text string `json:"text"`
			} `json:"options"`
		} `json:"suggest"`
	}
	if err := json.NewDecoder(res.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("응답 파싱 실패: %w", err)
	}
	var out []string
	for _, bucket := range parsed.Suggest["ac"] {
		for _, opt := range bucket.Options {
			out = append(out, opt.Text)
		}
	}
	return out, nil
}

func writeJSON(w http.ResponseWriter, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("응답 직렬화 실패: %v", err)
		http.Error(w, "서버 오류", http.StatusInternalServerError)
	}
}

func discard(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, body)
	_ = body.Close()
}

func docID(keyword string) string {
	normalized := strings.ToLower(strings.TrimSpace(keyword))
	sum := sha1.Sum([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

const indexMapping = `
{
  "settings": {
    "analysis": {
      "filter": {
        "autocomplete_filter": {
          "type": "edge_ngram",
          "min_gram": 1,
          "max_gram": 20
        }
      },
      "analyzer": {
        "autocomplete": {
          "type": "custom",
          "tokenizer": "standard",
          "filter": [
            "lowercase",
            "autocomplete_filter"
          ]
        }
      }
    }
  },
  "mappings": {
    "properties": {
      "keyword": { "type": "keyword" },
      "suggest": {
        "type": "completion",
        "analyzer": "autocomplete",
        "preserve_separators": true
      },
      "meta": { "type": "object", "enabled": true }
    }
  }
}`
