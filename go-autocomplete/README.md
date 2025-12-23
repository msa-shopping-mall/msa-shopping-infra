# Go Autocomplete Service

간단한 검색어 자동완성 API입니다. Elasticsearch 8.x `completion` 필드와 edge_ngram 기반 분석기를 사용합니다.

## 실행
```bash
cd go-autocomplete
export ELASTICSEARCH_URL=http://localhost:9200  # 기본값 동일
go run ./...
```

환경 변수
- `ELASTICSEARCH_URL` (기본 `http://localhost:9200`)
- `ELASTICSEARCH_USERNAME` / `ELASTICSEARCH_PASSWORD` (보안 활성화 시)
- `PORT` (기본 8080)

## API
- `POST /keywords`  
  ```json
  {
    "keyword": "iphone 15",
    "weight": 3,
    "meta": { "category": "mobile" }
  }
  ```

- `GET /suggest?q=iph`  
  ```json
  { "suggestions": ["iphone 15"] }
  ```

## Docker Compose 연동 예시
`docker-compose.yml`에 아래 서비스를 추가하면 ELK 네트워크에서 바로 붙일 수 있습니다.
```yaml
  autocomplete:
    build: ./go-autocomplete
    environment:
      - ELASTICSEARCH_URL=http://elasticsearch:9200
    ports:
      - "8080:8080"
    depends_on:
      - elasticsearch
    networks:
      - elk
```

간단한 `Dockerfile` (필요 시):
```dockerfile
FROM golang:1.21 AS builder
WORKDIR /app
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o autocomplete

FROM gcr.io/distroless/base
WORKDIR /app
COPY --from=builder /app/autocomplete .
EXPOSE 8080
ENTRYPOINT ["./autocomplete"]
```

## Logstash 파이프라인
`logstash/pipeline/autocomplete.conf`에 아래를 넣으면 `/logs/products.jsonl`에서 읽어 자동완성 인덱스에 업서트합니다.
```conf
input {
  file {
    path => "/logs/products.jsonl"
    start_position => "beginning"
    sincedb_path => "/tmp/sincedb"
    codec => "json"
  }
}

filter {
  mutate { rename => { "name" => "keyword" } }
  mutate { add_field => { "[suggest][input]" => "%{keyword}" } }
}

output {
  elasticsearch {
    hosts => [ "http://elasticsearch:9200" ]
    index => "autocomplete"
    document_id => "%{id}"
    action => "update"
    doc_as_upsert => true
  }
}
```
