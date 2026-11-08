# BangDB

Go로 작성된 **경량 임베디드 관계형 데이터베이스**입니다. 외부 의존성 0개, 단일 파일 저장, 프로젝트에 라이브러리로 바로 넣어 쓰는 것을 목표로 합니다 (SQLite와 같은 사용 방식).

- 순수 Go, 표준 라이브러리만 사용 (`go.mod`에 require 없음)
- 단일 파일 + WAL(`<파일>-wal`) 기반 **원자적·내구성 있는 커밋과 크래시 복구**, 페이지 체크섬, 무결성 검사(`IntegrityCheck`)
- `:memory:` 인메모리 모드
- SQL: `CREATE/DROP TABLE`, `ALTER TABLE ADD/RENAME/DROP COLUMN`, `RENAME TO`, `CREATE [UNIQUE] INDEX`(복합·부분 `WHERE`), `CREATE TRIGGER`, `CREATE VIEW`, `INSERT/SELECT/UPDATE/DELETE`, `JOIN`(INNER/LEFT/CROSS), `GROUP BY/HAVING`, `ORDER BY`, `LIMIT/OFFSET`, `DISTINCT`, `CASE`, `IN`, `BETWEEN`, `LIKE`, `COLLATE`, 집계/스칼라/날짜 함수, `EXPLAIN`, `VACUUM`
  - 서브쿼리(스칼라·`EXISTS`·`IN (SELECT)`·`FROM (SELECT)`, 상관 포함), `UNION [ALL]`/`INTERSECT`/`EXCEPT`
  - UPSERT(`ON CONFLICT … DO NOTHING / DO UPDATE`), `INSERT OR IGNORE/REPLACE`, `INSERT … SELECT`, `RETURNING`
  - 제약: 복합 `PRIMARY KEY`/`UNIQUE`, `NOT NULL`, `DEFAULT`, `CHECK`, `FOREIGN KEY`(`CASCADE`/`SET NULL`/`RESTRICT`)
  - 파라미터: `?`, `?N`, `:name`, `@name`, `$name`
  - 윈도우 함수(`OVER (PARTITION BY … ORDER BY … ROWS/RANGE …)`), JSON 함수와 `->`/`->>` 연산자
  - 트리거: `CREATE TRIGGER … BEFORE|AFTER INSERT|UPDATE [OF cols]|DELETE ON t [WHEN cond] BEGIN … END`, `NEW`/`OLD`, `RAISE(ABORT, '메시지')`·`RAISE(IGNORE)`
- B+트리 기반 테이블/인덱스(삭제 시 병합, 긴 키 지원), 비용 기반 접근 경로 선택: rowid·인덱스 동등/범위/IN, OR → 인덱스 합집합, 인덱스 순서로 ORDER BY 생략, 해시 조인, covering index, `ANALYZE` 통계 기반 조인 순서
- 디코드된 노드 캐시·플랜 캐시, 메모리 한도를 넘는 정렬·그룹핑은 임시 파일로 처리
- 교체 가능한 저장 방식 인터페이스(`internal/store`, `CREATE TABLE … USING <kind>`) — 기본은 B+트리, **`USING memory`** 인메모리 테이블, **`USING timeseries`** 시계열 테이블
- 트랜잭션: `BEGIN/COMMIT/ROLLBACK`, 문장 단위 원자성, **스냅샷 읽기** — 읽기와 쓰기가 서로를 기다리지 않음
- 스트리밍 결과 커서, `context` 기반 쿼리 취소·타임아웃, 그룹 커밋
- **온라인 백업**(서비스 중단 없이 일관된 백업 파일 생성)과 **primary → replica 미러링**(읽기 스케일 아웃, 자동 재접속, 장애 조치 승격)
- `database/sql` 드라이버(`bangdb`) 및 대화형 CLI 제공, **서버 모드**(TCP/TLS, 인증, `bangdb://` DSN)
- 운영: `db.Stats()`, 슬로우 쿼리 훅, `EXPLAIN ANALYZE`, SQL 덤프·SQLite 덤프 가져오기·CSV, **저장 데이터 암호화**(AES-256-XTS), 반동기 복제·펜싱·**특정 시점 복구(PITR)**

## 설치

```bash
go get github.com/minaryhub/bangdb
```

안드로이드 앱에 내장: [docs/ANDROID.md](docs/ANDROID.md) (`.aar`, Kotlin/Java API `com.knoxxr.bangdb`).

## 사용법

### 네이티브 API

```go
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/minaryhub/bangdb"
)

func main() {
	db, err := bangdb.Open("app.db") // 인메모리: bangdb.Open(":memory:")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS users (
			id    INTEGER PRIMARY KEY,
			name  TEXT NOT NULL,
			email TEXT UNIQUE,
			age   INT DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS idx_users_age ON users (age);
	`)
	if err != nil {
		log.Fatal(err)
	}

	res, _ := db.Exec(`INSERT INTO users (name, email, age) VALUES (?, ?, ?)`, "kim", "kim@example.com", 31)
	fmt.Println("new id:", res.LastInsertID)

	// 트랜잭션: fn이 nil을 반환하면 커밋, 에러면 롤백
	err = db.Update(func(tx *bangdb.Tx) error {
		_, err := tx.Exec(`UPDATE users SET age = age + 1 WHERE id = ?`, res.LastInsertID)
		return err
	})

	// 결과는 스트리밍됩니다. 끝까지 읽지 않을 때는 반드시 Close 하세요.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rows, err := db.QueryContext(ctx, `SELECT id, name, age FROM users WHERE age > ? ORDER BY age DESC`, 20)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name string
		var age int
		rows.Scan(&id, &name, &age)
		fmt.Println(id, name, age)
	}
	if err := rows.Err(); err != nil { // 예: 타임아웃
		log.Fatal(err)
	}

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&count)
}
```

### 스키마 마이그레이션

```go
applied, err := db.Migrate(ctx, []bangdb.Migration{
    {Version: 1, SQL: `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`},
    {Version: 2, SQL: `ALTER TABLE users ADD COLUMN email TEXT`},
    {Version: 3, Func: func(tx *bangdb.Tx) error { _, err := tx.Exec(`UPDATE users SET email = ''`); return err }},
})
```

`PRAGMA user_version`에 마지막으로 적용한 버전이 기록되고, 각 단계는 자기 트랜잭션에서 실행되어 실패하면 그 단계만 취소됩니다. 그 밖의 `PRAGMA`: `table_info(t)`, `index_list(t)`, `index_info(i)`, `table_list`, `integrity_check`, `foreign_keys`, `journal_mode`, `page_count`, `page_size`.

### Prepared statement

```go
get, _ := db.Prepare("SELECT name FROM users WHERE id = ?") // 파싱 1회, 플랜 보관 (동시 사용 가능)
var name string
get.QueryRow(42).Scan(&name)
```

### database/sql 드라이버

```go
import (
	"database/sql"
	_ "github.com/minaryhub/bangdb/driver"
)

db, err := sql.Open("bangdb", "app.db")
```

같은 `*sql.DB`의 모든 커넥션은 하나의 엔진 인스턴스를 공유합니다 (`:memory:`도 커넥션 간 공유됨).

### 백업과 미러링

```go
db.Backup("backup.db")   // 온라인 백업 (결과는 그대로 열 수 있는 DB 파일)

// primary
p := replication.NewPrimary(db, replication.PrimaryOptions{Token: "secret"})
go p.ListenAndServe(":7070")

// replica (다른 서버): 처음엔 스냅샷을 받고, 이후 커밋을 실시간 미러링
rdb, _ := bangdb.Open("replica.db")
r := replication.StartReplica(rdb, "primary:7070", replication.ReplicaOptions{Token: "secret"})
rdb.Query("SELECT ...")   // 읽기 분산; 쓰기는 ErrReadOnly
r.Promote()               // 장애 조치: 쓰기 가능한 primary로 승격
```

자세한 내용: [docs/REPLICATION.md](docs/REPLICATION.md)

### 서버 모드 (여러 프로세스가 공유할 때)

```go
// 서버: DB를 연 프로세스가 TCP/TLS로 제공
srv := server.New(db, server.Options{Password: "s3cret", MaxConns: 100, IdleTimeout: 10 * time.Minute})
go srv.ListenAndServe("127.0.0.1:7432") // 또는 ListenAndServeTLS(addr, tlsConfig)

// 클라이언트: 직접
c, _ := client.Dial(ctx, "db.internal:7432", &client.Options{User: "app", Password: "s3cret"})
rows, _ := c.Query(ctx, "SELECT id, name FROM users WHERE age > ?", 30)

// 클라이언트: database/sql (커넥션 풀)
sdb, _ := sql.Open("bangdb", "bangdb://app:s3cret@db.internal:7432?tls=1")
```

- 연결마다 세션 하나: `BEGIN/COMMIT`이 그대로 동작하고, 연결이 끊기면 열린 트랜잭션은 롤백됩니다. 결과는 스트리밍되며 `Rows.Close`나 `ctx` 취소가 서버의 실행 중인 문장을 중단합니다. 오류 종류는 유지됩니다(`errors.Is(err, bangdb.ErrConstraint)` 등).
- 인증은 비밀번호 또는 `Options.Authenticate(user, password)` 콜백, 암호화는 TLS. 루프백 왕복 지연은 단건 조회 약 44 µs.
- CLI: `bangdb -listen :7432 -password s3cret [-tls-cert c.pem -tls-key k.pem] app.db`, 원격 셸 `bangdb -connect bangdb://app:s3cret@host:7432`.

### HTTP/JSON API (다른 언어·브라우저·웹 도구용)

```bash
bangdb -http :18080 -password s3cret [-http-cors https://admin.example.com] [-http-readonly] app.db
```

```bash
curl -H 'Authorization: Bearer s3cret' -d '{"sql":"INSERT INTO users (name) VALUES (?)","args":["kim"]}' localhost:18080/exec
curl -H 'Authorization: Bearer s3cret' -d '{"sql":"SELECT * FROM users WHERE id = :id","args":{"id":1}}' localhost:18080/query
curl -H 'Authorization: Bearer s3cret' -d '{"statements":[{"sql":"..."},{"sql":"..."}]}' localhost:18080/batch
```

| 엔드포인트 | 내용 |
|---|---|
| `POST /query` | SELECT·EXPLAIN만. `{"columns","types","rows"}`를 스트리밍으로 반환 |
| `POST /exec` | 모든 문장. `{"rowsAffected","lastInsertId"}` (+ RETURNING이면 `columns`,`rows`) |
| `POST /batch` | 여러 문장을 **한 트랜잭션**으로 (하나라도 실패하면 전부 취소, 실패 위치 `statement`) |
| `GET /health`, `GET /stats` | 상태·버전, `db.Stats()` |
| `/v1/health`, `/v1/tables`, `/v1/tables/{name}`, `POST /v1/query`, `/v1/stats` | [BangDB Manager](https://github.com/knoxxr/BangDB.Manager)의 "BangDB (Go)" 연결이 쓰는 API. `/v1/query`는 `{"sql","args","mode":"auto|exec|query","limit"}` → `{"columns","types","rows","rows_affected","last_insert_id","truncated","ms"}`, 스크립트 안의 `BEGIN…COMMIT` 가능, BLOB은 `{"blob":…}`. `/v1/health`는 인증 없이 버전만 |

- 인자는 배열(`?`) 또는 객체(`:name`). 값: `null`, 숫자(정수는 정확히 유지), 문자열, `true/false`(1/0), BLOB은 `{"$blob":"<base64>"}`(결과도 같은 형식).
- HTTP Basic은 계정 파일 사용자, 또는 **아무 사용자 이름 + `-password` 값**(관리자)으로 인증됩니다(BangDB Manager가 이렇게 보냄).
- 오류는 `{"error","code"}` + HTTP 상태: 400 SQL 오류, 401 인증, 403 읽기 전용, 409 제약 위반, 499 중단, 503 쓰기 락 대기 초과.
- 요청마다 독립(세션 없음)이라 `BEGIN/COMMIT`은 쓸 수 없고 트랜잭션은 `/batch`로. 성능·세션이 필요하면 바이너리 프로토콜(`-listen`)을 쓰세요. `-tls-cert/-tls-key`를 주면 HTTPS. Go에서는 `httpapi.New(db, opts)`를 `http.Handler`로 붙일 수 있습니다.

### 저장 데이터 암호화

```go
key := loadKeyFromKMS() // 32바이트 무작위 키 (DB 밖에 보관)
db, err := bangdb.OpenWithOptions("app.db", &bangdb.Options{EncryptionKey: key})
// 키 없이 열면 bangdb.ErrEncrypted, 다른 키면 bangdb.ErrWrongKey
db.BackupWithKey(ctx, "rekeyed.db", newKey) // 다른 키(또는 nil = 평문)로 복사 → 암호화·해제·키 교체
```

- 데이터 파일과 WAL의 모든 페이지(헤더 페이지 0 제외)를 **AES-256-XTS**(디스크 암호화 표준, 페이지 번호가 tweak)로 암호화합니다. 메모리 안의 페이지와 복제 스트림은 평문이며, 백업·VACUUM은 같은 키를 유지합니다. 복제본은 자기 키(또는 평문)를 따로 가질 수 있습니다.
- 헤더 페이지에는 페이지 수·루트·커밋 번호·DB ID와 키 확인 값만 평문으로 남습니다(테이블 데이터·스키마 없음).
- 무결성: 암호화된 페이지 안의 CRC가 잘못된 키·손상을 검출하지만 **암호학적 MAC은 아닙니다**(XTS는 위·변조 방지를 보장하지 않음). 디스크에서 읽는 페이지는 복호화 비용이 더해집니다(캐시 적중 시 없음).
- 키 관리(KMS, 패스프레이즈에서 키 유도)는 애플리케이션 몫입니다.

### 덤프·이관

```go
f, _ := os.Create("backup.sql")
db.Dump(ctx, f)                         // 스냅샷에서 SQL 스크립트로 (쓰기 차단 없음)

res, err := db.ImportSQL(ctx, file)     // Dump 결과 또는 SQLite `.dump` 결과를 한 트랜잭션으로 가져오기
n, err := db.ImportCSV(ctx, "people", csvFile, &bangdb.CSVOptions{Create: true}) // 헤더 행으로 컬럼 매칭
n, err = db.ExportCSV(ctx, w, nil, "SELECT * FROM people WHERE age > ?", 30)
```

- `ImportSQL`은 SQLite 덤프를 그대로 받습니다: `PRAGMA`/`BEGIN`/`COMMIT` 무시, `sqlite_sequence`로 AUTOINCREMENT 값 복원, 부모보다 먼저 만든 자식 테이블은 부모가 생길 때까지 대기, `WITHOUT ROWID`/`STRICT` 제거, 트리거는 건너뛰고 결과에 표시. 외래 키는 마지막에 한 번 검사하며, 실패하면 아무것도 반영하지 않습니다.
- CLI: `.dump [FILE]`, `.read FILE`, `.import FILE TABLE`, `.export FILE QUERY`.

### 설정 파일·환경 변수 (CLI·서버 운영)

모든 옵션은 명령줄 플래그(`-cache-size`), 환경 변수(`BANGDB_CACHE_SIZE`), 설정 파일(`-config bangdb.conf` 또는 `BANGDB_CONFIG`) 중 어디서든 줄 수 있습니다. 우선순위는 **플래그 > 환경 변수 > 설정 파일 > 기본값**.

```ini
# bangdb.conf
database            = /var/lib/bangdb/app.db
listen              = :7432
http                = :18080
http_cors           = https://admin.example.com
password_file       = /etc/bangdb/password       # 비밀번호를 프로세스 목록에 노출하지 않음
encryption_key_file = /etc/bangdb/db.key         # 32바이트 키(원시 또는 hex)
tls_cert            = /etc/bangdb/cert.pem
tls_key             = /etc/bangdb/key.pem
query_timeout       = 30s                        # 서버 모드 기본 60s, 0이면 제한 없음
busy_timeout        = 5s
slow_query          = 500ms                      # 느린 문장을 stderr에 기록
cache_size          = 256MB
max_conns           = 200
idle_timeout        = 10m
http_max_concurrent = 64
```

```bash
bangdb -config bangdb.conf                # 서버 실행
bangdb -config bangdb.conf -print-config  # 적용된 설정 확인 (비밀 값은 가림)
```

- 서버 모드(`listen`/`http`)에서는 `query_timeout`을 지정하지 않으면 **60초**가 적용되어, 폭주 쿼리 하나가 서버를 붙잡지 못합니다. HTTP 서버는 헤더 10초·요청 60초·유휴 2분 제한이 기본입니다.
- 비밀번호 없이 서버를 띄우면 경고를 출력합니다.
- 같은 DB 파일은 한 프로세스만 열 수 있습니다(리눅스·macOS·**Windows** 모두 파일 잠금, 두 번째 열기는 `bangdb.ErrLocked`).

### 계정·권한, 모니터링, 정기 백업, 웹 콘솔 (서버 운영)

```bash
bangdb -add-user app:write -users-file users      # 비밀번호를 입력받아 해시로 저장 (역할: read / write / admin)
bangdb -add-user report:read -users-file users
```

```ini
# bangdb.conf
users_file      = /etc/bangdb/users      # 서버(-listen)와 HTTP API 모두 적용
http            = :18080                 # 브라우저로 http://서버:18080/ → 웹 콘솔
backup_dir      = /var/backups/bangdb    # 시작 시 1회 + 주기적으로 백업, 열어서 무결성 검사까지
backup_interval = 6h
backup_keep     = 28
log_format      = json                   # 접속·인증 실패·요청·느린 쿼리·백업 결과를 stderr로
```

| 역할 | 허용 |
|---|---|
| `read` | SELECT, EXPLAIN, PRAGMA 조회 |
| `write` | + INSERT/UPDATE/DELETE, 트랜잭션 |
| `admin` | + CREATE/DROP/ALTER, VACUUM, ANALYZE, PRAGMA 설정 |

- 계정 파일에는 비밀번호 대신 PBKDF2-SHA256 해시만 저장됩니다. 클라이언트는 `bangdb://app:비밀번호@host:7432`, HTTP는 Basic 인증(`curl -u app:비밀번호`). `password`(토큰)는 관리자 권한으로 함께 쓸 수 있습니다.
- `GET /metrics`: Prometheus 형식(커밋·오류·락 대기·WAL·체크포인트·캐시·스냅샷 지연·서버 연결 수 등).
- 웹 콘솔(`/`): SQL 실행(Ctrl+Enter), 테이블 목록, 상태. 콘솔 페이지 자체는 데이터가 없고, 실행은 로그인한 계정의 권한으로만 됩니다. 끄려면 Go에서 `httpapi.Options{NoConsole: true}`.
- Go에서는 `db.StartBackups(bangdb.BackupSchedule{...})`, `db.VerifyBackup(ctx, path)`.

### 관측성

```go
db, _ := bangdb.OpenWithOptions("app.db", &bangdb.Options{
    SlowQueryThreshold: 200 * time.Millisecond,
    OnSlowQuery: func(q bangdb.SlowQuery) { log.Printf("slow %s %v: %s", q.Duration, q.Args, q.SQL) },
})
s := db.Stats() // 문장·오류·슬로우 수, 커밋 수와 누적 시간, 쓰기 락 대기, WAL 크기, 체크포인트,
                // 페이지/노드 캐시 적중, 열린 스냅샷과 그 지연(커밋 수), 열린 커서
```

- `EXPLAIN ANALYZE SELECT …`: 쿼리를 실제로 실행하고 조인 단계별 `loops`(스캔 횟수)·`scanned`(본 행)·`rows`(조건 통과 행)와 결과 행 수·실행 시간을 보여 줍니다 (SELECT만).
- CLI에서는 `.stats`.

### CLI

```bash
go install github.com/minaryhub/bangdb/cmd/bangdb@latest

bangdb app.db                         # 대화형 셸 (.help, .tables, .schema, .timer on, .backup FILE, .check)
bangdb -serve :7070 app.db            # primary로 replica 접속 허용
bangdb -replica-of host:7070 r.db     # 읽기 전용 replica (.repl 상태, .promote 승격)
bangdb app.db "SELECT COUNT(*) FROM users"
cat schema.sql | bangdb app.db
```

```
bangdb> EXPLAIN SELECT * FROM users u JOIN orders o ON o.user_id = u.id WHERE u.id = 1;
plan
------------------------------------------------
SEARCH users USING ROWID (rowid=?)
  SEARCH orders USING INDEX idx_orders_user (user_id=?)
```

## 인메모리 테이블

```sql
CREATE TABLE sessions (token TEXT PRIMARY KEY, user_id INT, hits INT) USING memory;
```

- 행과 인덱스를 페이지가 아닌 메모리 자료구조에 보관합니다. 쓰기가 2.6~5배 빠르고 메모리 사용량이 B+트리(`:memory:`)의 약 40%입니다.
- 트랜잭션·롤백·스냅샷 읽기·인덱스·조인·FK는 일반 테이블과 같습니다.
- **데이터는 프로세스가 살아 있는 동안만** 유지됩니다(MySQL MEMORY 엔진과 같음). 테이블 정의는 남고 재시작하면 빈 테이블이 됩니다. 백업·복제 대상이 아닙니다.
- `USING memory WITH (persist = true)`: 정상 종료(`Close`) 때 행을 `<파일>-mem`에 저장하고 다음 `Open`에서 복원합니다(인덱스 포함, DB가 암호화돼 있으면 파일도 암호화). 그 사이 DB가 바뀌었거나(예: 크래시 후 복구) 비정상 종료했다면 복원하지 않습니다 — 캐시를 재시작 후에도 따뜻하게 유지하는 용도.
- 일반 테이블이 메모리 테이블을 외래 키로 참조할 수는 없습니다(반대는 가능).
- 보존이 필요하면 `INSERT INTO 일반테이블 SELECT * FROM 메모리테이블`로 저장하세요. `:memory:` DB 전체는 `db.Backup(path)`로 파일에 저장할 수 있습니다(메모리 테이블 제외).

## 시계열 테이블

```sql
CREATE TABLE cpu (ts INTEGER NOT NULL, host TEXT, usage REAL)
  USING timeseries WITH (time_column = ts, time_unit = 's', retention = '30 days');

INSERT INTO cpu VALUES (unixepoch('now'), 'web-1', 42.5);

SELECT time_bucket('5 minutes', ts) AS t, host, avg(usage)
FROM cpu WHERE ts >= unixepoch('now', '-1 hour')
GROUP BY t, host ORDER BY t;
```

- 행이 시간 순으로 저장되어 시간 조건과 `ORDER BY ts`가 범위 스캔으로 처리됩니다(`EXPLAIN`: `USING TIME RANGE`).
- 오래된 행은 열 단위로 압축된 블록으로 저장됩니다(일반 테이블 대비 약 1/5 크기).
- `retention`을 주면 가장 최근 데이터 시각 기준으로 보존 기간이 지난 블록이 자동 삭제됩니다. 시간 조건만 있는 `DELETE`도 블록 단위로 빠르게 처리됩니다.
- 옵션: `time_column`(필수, INTEGER epoch), `time_unit`(`s`·`ms`·`us`), `retention`(`'30 days'` 등), `block_rows`(기본 1024).
- 제약: 보조 인덱스·PRIMARY KEY·UNIQUE를 둘 수 없습니다. 태그(호스트 등) 조건은 시간 범위 안에서 거릅니다.
- 일반 테이블과 똑같이 트랜잭션·조인·백업·복제가 됩니다.

## 데이터 타입

| 선언 타입 (예)                                  | 저장 타입 |
|-------------------------------------------------|-----------|
| `INTEGER`, `INT`, `BIGINT`, `BOOLEAN`           | INTEGER (int64) |
| `DECIMAL(p,s)`, `NUMERIC(p,s)` (자릿수 명시)     | **DECIMAL** (정확한 십진수, 최대 18자리) |
| `REAL`, `FLOAT`, `DOUBLE`, `NUMERIC`, `DECIMAL` (자릿수 없음) | REAL (float64) |
| `TEXT`, `VARCHAR(n)`, `CHAR`, `DATE`, `TIMESTAMP`, `JSON`, `UUID` | TEXT |
| `BLOB`, `BINARY`, `BYTEA`                       | BLOB |
| (타입 생략)                                     | ANY (변환 없음) |

- 값은 컬럼 타입으로 **손실 없는 경우에만** 변환됩니다 (`'42'` → INT 42는 허용, `1.5` → INT는 에러).
- `INTEGER PRIMARY KEY`는 rowid 자체입니다 (생략 시 자동 증가). 그 외 PK는 자동 UNIQUE 인덱스가 생성됩니다.
- 모든 테이블에 숨은 `rowid` 컬럼이 있습니다.
- Go `time.Time`은 TEXT(RFC3339) 로 저장되고 `Scan(&t)`로 다시 읽을 수 있습니다.
- **금액은 `DECIMAL(p,s)`로**: `0.1 + 0.2`가 정확히 `0.30`이 됩니다(REAL은 `0.30000000000000004`). 저장 시 소수 `s`자리로 반올림(0.5는 0에서 먼 쪽)하고, 전체 `p`자리를 넘으면 오류입니다. DECIMAL끼리·정수·짧은 실수 리터럴과의 연산과 `SUM`/`AVG`는 정확하게 계산되고, 나눗셈은 소수 6자리를 더해 반올림합니다. Go에서는 `bangdb.Decimal`(`Scan(&d)`, `ParseDecimal("12.50")`), `database/sql`에서는 정확한 문자열로 읽힙니다. 이전 버전에서 만든 `DECIMAL(p,s)` 컬럼은 REAL 그대로입니다.

## 함수

- 집계: `COUNT(*)`, `COUNT([DISTINCT] x)`, `SUM`, `AVG`, `MIN`, `MAX`, `TOTAL`, `GROUP_CONCAT`
- 스칼라: `LOWER`, `UPPER`, `LENGTH`, `SUBSTR`, `TRIM/LTRIM/RTRIM`, `REPLACE`, `INSTR`, `ABS`, `ROUND`, `COALESCE`, `IFNULL`, `NULLIF`, `MIN/MAX(a, b, ...)`, `TYPEOF`, `CAST(x AS type)`
- 날짜: `date`, `time`, `datetime`, `strftime`, `julianday`, `unixepoch`, `time_bucket`(수식어 `'+1 day'`, `'start of month'`, `'weekday N'`, `'unixepoch'`, `'localtime'` 등), `CURRENT_TIMESTAMP`, `CURRENT_DATE`, `CURRENT_TIME`, `NOW()`
- 윈도우: `ROW_NUMBER`, `RANK`, `DENSE_RANK`, `PERCENT_RANK`, `CUME_DIST`, `NTILE`, `LAG`, `LEAD`, `FIRST_VALUE`, `LAST_VALUE`, `NTH_VALUE`, 그리고 모든 집계 함수(`SUM(x) OVER (…)`). 프레임은 `ROWS BETWEEN n PRECEDING AND m FOLLOWING` 등(`RANGE`는 오프셋 없는 형태만). 그룹 질의 위에서도 동작(`RANK() OVER (ORDER BY SUM(x) DESC)`)
- JSON (문서는 TEXT): `json`, `json_valid`, `json_type`, `json_extract`, `json_array_length`, `json_array`, `json_object`, `json_set/insert/replace/remove`, `json_patch`, `json_quote`, 집계 `json_group_array/json_group_object`, 연산자 `doc -> 'path'`(JSON) / `doc ->> 'path'`(SQL 값). 경로는 `$.a.b[0]`, `$[#-1]`, `$."키"`; `->`의 오른쪽은 `'$…'` 경로·키 이름·배열 번호. JSON 함수나 `->`의 결과를 다른 JSON 생성 함수에 넣으면 문자열이 아니라 JSON으로 들어감
- 비교 규칙: `COLLATE NOCASE`(유니코드 대소문자 무시), `RTRIM`, `BINARY` — 컬럼에 지정하면 인덱스도 같은 규칙을 따름

## 스키마 예

```sql
CREATE TABLE dept (id INTEGER PRIMARY KEY, code TEXT UNIQUE COLLATE NOCASE, name TEXT);
CREATE TABLE emp (
  id      INTEGER PRIMARY KEY,
  dept_id INT REFERENCES dept ON DELETE CASCADE,
  email   TEXT NOT NULL,
  salary  INT CHECK (salary >= 0),
  UNIQUE (dept_id, email)
);
CREATE INDEX emp_dept ON emp (dept_id);        -- FK 컬럼에는 인덱스를 권장
CREATE VIEW payroll AS SELECT d.name, sum(e.salary) AS total FROM emp e JOIN dept d ON d.id = e.dept_id GROUP BY d.name;

INSERT INTO emp (dept_id, email, salary) VALUES (1, 'kim@example.com', 100)
  ON CONFLICT (dept_id, email) DO UPDATE SET salary = excluded.salary
  RETURNING id;
```

## 동시성과 내구성

- **스냅샷 읽기**: 각 조회는 시작 시점의 커밋 상태를 읽습니다. 오래 걸리는 조회가 있어도 커밋은 기다리지 않고, 조회는 그 사이의 커밋을 보지 않습니다.
- 쓰기 트랜잭션은 한 번에 하나입니다. 다른 쓰기는 `BusyTimeout`(기본 5초)까지 기다린 뒤 `ErrBusy`를 반환합니다.
- **커밋**: 변경 페이지를 WAL에 추가하고 fsync 1회 후 공개합니다. WAL이 1,000페이지를 넘으면 열린 스냅샷이 허용하는 범위까지 DB 파일로 옮기는 체크포인트가 자동 실행됩니다.
- **크래시 복구**: 다음 `Open` 시 WAL의 완전한 커밋만 재적용하고 잘린 커밋은 버립니다. `kill -9` 반복 테스트로 "응답한 커밋은 모두 남고, 진행 중이던 커밋은 전부 반영되거나 전혀 반영되지 않음"을 검증합니다.
- **그룹 커밋**: 동시에 커밋하는 트랜잭션들의 WAL fsync를 한 번으로 묶습니다 (32 고루틴 동시 자동 커밋 ~1만 건/초).
- **손상 감지**: 모든 페이지에 CRC32가 있어 디스크에서 읽을 때 검증합니다. `db.IntegrityCheck()`(CLI `.check`)는 트리 구조, 페이지 누수/중복 사용, 인덱스와 테이블 일치, 외래 키를 검사합니다.
- **공간 회수**: 삭제로 빈 페이지는 같은 파일 안에서 재사용됩니다. `VACUUM`(또는 `db.Vacuum(ctx)`)은 파일을 새로 압축해 교체합니다 — 읽기는 계속되고 쓰기는 끝날 때까지 기다리며, 복제본은 스냅샷으로 다시 동기화됩니다.
- **메모리 상한**: `Options.CacheSize`(기본 32 MiB, 페이지·노드 캐시), `Options.SortMemory`(기본 64 MiB, 초과 시 `Options.TempDir`에 임시 파일), `Options.TxMemory`(기본 64 MiB: 쓰기 트랜잭션이 바꾼 페이지가 이를 넘으면 임시 파일로 옮김 — 메모리보다 큰 트랜잭션 가능, 암호화 DB는 임시 파일도 암호화).
- **취소·타임아웃**: `ExecContext/QueryContext`의 context나 `Options.QueryTimeout`으로 실행 중인 문장을 중단하고 롤백합니다.
- 한 파일은 한 프로세스만 열 수 있습니다 (Unix `flock`).

## 성능 (참고치)

4코어 리눅스 컨테이너, `go test -bench .` 기준:

| 작업 | 시간 |
|------|------|
| 트랜잭션 내 INSERT (인덱스 1개 포함) | ~18 µs/행 |
| 자동 커밋 INSERT, 32 고루틴 동시 (그룹 커밋) | ~1만 건/초 |
| PK 조회 (10만 행, `QueryRow`) | ~2.7 µs |
| 보조 인덱스 조회 (10만 행) | ~3.6 µs |
| 인덱스 범위 100행 | ~70 µs |
| 10만 행 전체 스캔 + GROUP BY | ~27 ms |
| 인덱스 없는 조인 1,500 × 200,000 (해시 조인) | ~0.25 s |
| 20만 행 `SELECT *` 스트리밍 | 힙 증가 ~6 MB |

대량 삽입은 반드시 트랜잭션으로 묶으세요.

## 현재 제약 사항

> 보완 항목과 개발 계획은 [docs/ROADMAP.md](docs/ROADMAP.md)를 보세요.

- 식(expression) 인덱스, 이름 있는 윈도(`WINDOW w AS`), `json_each` 같은 테이블 값 함수, `INSTEAD OF` 트리거 미지원. 외래 키 동작(CASCADE 등)으로 바뀐 행은 트리거를 실행하지 않음
- 윈도우 함수가 있는 조회는 결과 행을 메모리에 모은 뒤 계산
- 조인 순서는 비용 추정(테이블 크기, `ANALYZE` 통계)으로 정하되 `LEFT JOIN`과 `ORDER BY … LIMIT` 질의는 쓴 순서를 따름 → `EXPLAIN`으로 확인. 통계는 평균값이라 값 분포가 치우친 컬럼은 잘못 추정할 수 있음
- 외래 키는 항상 적용되며 즉시 검사 (지연 제약·`SET DEFAULT` 없음). 부모 테이블을 먼저 만들어야 함
- 뷰는 읽기 전용이며 조회할 때마다 실행됨
- 집합 연산(`UNION` 등)과 해시 조인의 빌드 측은 메모리에서 처리
- 열린 `Rows`·긴 조회는 그 시점 이후의 체크포인트(와 `VACUUM` 교체)를 막아 WAL이 커질 수 있음 → 다 읽은 `Rows`는 닫기
- 하나의 쓰기 트랜잭션에서 변경한 페이지는 커밋 전까지 메모리에 유지됨
- 파일 포맷 버전 3. 개발 중 버전(1, 2)으로 만든 파일은 열 수 없음

## 문서

- [호환성 정책](docs/COMPATIBILITY.md) · [변경 이력](CHANGELOG.md)

- [보완 검토와 개발 계획 (ROADMAP)](docs/ROADMAP.md)
- [아키텍처](docs/ARCHITECTURE.md)
- [백업과 미러링](docs/REPLICATION.md)
- [안드로이드 앱에 내장하기](docs/ANDROID.md)
- [시계열·인메모리 DB 확장 적합성 검토](docs/REVIEW-timeseries-inmemory.md)

## 빌드와 버전

```bash
scripts/build.sh            # 이 PC용 → dist/bangdb-<버전>-<os>-<arch>
scripts/build.sh all        # Windows·macOS·Linux 전부
scripts/build.sh install    # 버전을 넣어 go install
bangdb -version             # 예: bangdb 0.6.44 (1a2b3c4, 2026-10-26T23:30:00Z)
```

버전은 `VERSION` 파일의 기준 버전(주.부)에 **커밋 수**를 붙인 `주.부.커밋수`이므로, 커밋이 생길 때마다 빌드 버전이 올라갑니다. 커밋하지 않은 변경이 있으면 `-dirty`가 붙습니다. 기능 단계가 바뀌면 `VERSION`(예: 0.6 → 0.7)을 올리세요. 셸에서는 `.version`, 코드에서는 `bangdb.Version`/`bangdb.VersionString()`.

## 개발

```bash
go test ./...           # 단위/통합 테스트
go test -race ./...     # 레이스 검사
BANGDB_TEST_STORE=reversed go test .   # 저장 방식 인터페이스 우회 검사
BANGDB_TEST_STORE=memory go test .     # 모든 테이블을 인메모리로 실행
go test -bench . -run XXX .
```
