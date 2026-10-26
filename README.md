# BangDB

Go로 작성된 **경량 임베디드 관계형 데이터베이스**입니다. 외부 의존성 0개, 단일 파일 저장, 프로젝트에 라이브러리로 바로 넣어 쓰는 것을 목표로 합니다 (SQLite와 같은 사용 방식).

- 순수 Go, 표준 라이브러리만 사용 (`go.mod`에 require 없음)
- 단일 파일 + WAL(`<파일>-wal`) 기반 **원자적·내구성 있는 커밋과 크래시 복구**, 페이지 체크섬, 무결성 검사(`IntegrityCheck`)
- `:memory:` 인메모리 모드
- SQL: `CREATE/DROP TABLE`, `ALTER TABLE ADD/RENAME/DROP COLUMN`, `RENAME TO`, `CREATE [UNIQUE] INDEX`(복합), `CREATE VIEW`, `INSERT/SELECT/UPDATE/DELETE`, `JOIN`(INNER/LEFT/CROSS), `GROUP BY/HAVING`, `ORDER BY`, `LIMIT/OFFSET`, `DISTINCT`, `CASE`, `IN`, `BETWEEN`, `LIKE`, `COLLATE`, 집계/스칼라/날짜 함수, `EXPLAIN`, `VACUUM`
  - 서브쿼리(스칼라·`EXISTS`·`IN (SELECT)`·`FROM (SELECT)`, 상관 포함), `UNION [ALL]`/`INTERSECT`/`EXCEPT`
  - UPSERT(`ON CONFLICT … DO NOTHING / DO UPDATE`), `INSERT OR IGNORE/REPLACE`, `INSERT … SELECT`, `RETURNING`
  - 제약: 복합 `PRIMARY KEY`/`UNIQUE`, `NOT NULL`, `DEFAULT`, `CHECK`, `FOREIGN KEY`(`CASCADE`/`SET NULL`/`RESTRICT`)
  - 파라미터: `?`, `?N`, `:name`, `@name`, `$name`
- B+트리 기반 테이블/인덱스(삭제 시 병합, 긴 키 지원), 비용 기반 접근 경로 선택: rowid·인덱스 동등/범위/IN, OR → 인덱스 합집합, 인덱스 순서로 ORDER BY 생략, 해시 조인
- 디코드된 노드 캐시·플랜 캐시, 메모리 한도를 넘는 정렬·그룹핑은 임시 파일로 처리
- 교체 가능한 저장 방식 인터페이스(`internal/store`, `CREATE TABLE … USING <kind>`) — 기본은 B+트리, **`USING memory`** 인메모리 테이블
- 트랜잭션: `BEGIN/COMMIT/ROLLBACK`, 문장 단위 원자성, **스냅샷 읽기** — 읽기와 쓰기가 서로를 기다리지 않음
- 스트리밍 결과 커서, `context` 기반 쿼리 취소·타임아웃, 그룹 커밋
- **온라인 백업**(서비스 중단 없이 일관된 백업 파일 생성)과 **primary → replica 미러링**(읽기 스케일 아웃, 자동 재접속, 장애 조치 승격)
- `database/sql` 드라이버(`bangdb`) 및 대화형 CLI 제공

## 설치

```bash
go get github.com/knoxxr/bangdb
```

## 사용법

### 네이티브 API

```go
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/knoxxr/bangdb"
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

### database/sql 드라이버

```go
import (
	"database/sql"
	_ "github.com/knoxxr/bangdb/driver"
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

### CLI

```bash
go install github.com/knoxxr/bangdb/cmd/bangdb@latest

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
- 일반 테이블이 메모리 테이블을 외래 키로 참조할 수는 없습니다(반대는 가능).
- 보존이 필요하면 `INSERT INTO 일반테이블 SELECT * FROM 메모리테이블`로 저장하세요. `:memory:` DB 전체는 `db.Backup(path)`로 파일에 저장할 수 있습니다(메모리 테이블 제외).

## 데이터 타입

| 선언 타입 (예)                                  | 저장 타입 |
|-------------------------------------------------|-----------|
| `INTEGER`, `INT`, `BIGINT`, `BOOLEAN`           | INTEGER (int64) |
| `REAL`, `FLOAT`, `DOUBLE`, `NUMERIC`, `DECIMAL` | REAL (float64) |
| `TEXT`, `VARCHAR(n)`, `CHAR`, `DATE`, `TIMESTAMP`, `JSON`, `UUID` | TEXT |
| `BLOB`, `BINARY`, `BYTEA`                       | BLOB |
| (타입 생략)                                     | ANY (변환 없음) |

- 값은 컬럼 타입으로 **손실 없는 경우에만** 변환됩니다 (`'42'` → INT 42는 허용, `1.5` → INT는 에러).
- `INTEGER PRIMARY KEY`는 rowid 자체입니다 (생략 시 자동 증가). 그 외 PK는 자동 UNIQUE 인덱스가 생성됩니다.
- 모든 테이블에 숨은 `rowid` 컬럼이 있습니다.
- Go `time.Time`은 TEXT(RFC3339) 로 저장되고 `Scan(&t)`로 다시 읽을 수 있습니다.

## 함수

- 집계: `COUNT(*)`, `COUNT([DISTINCT] x)`, `SUM`, `AVG`, `MIN`, `MAX`, `TOTAL`, `GROUP_CONCAT`
- 스칼라: `LOWER`, `UPPER`, `LENGTH`, `SUBSTR`, `TRIM/LTRIM/RTRIM`, `REPLACE`, `INSTR`, `ABS`, `ROUND`, `COALESCE`, `IFNULL`, `NULLIF`, `MIN/MAX(a, b, ...)`, `TYPEOF`, `CAST(x AS type)`
- 날짜: `date`, `time`, `datetime`, `strftime`, `julianday`, `unixepoch`(수식어 `'+1 day'`, `'start of month'`, `'weekday N'`, `'unixepoch'`, `'localtime'` 등), `CURRENT_TIMESTAMP`, `CURRENT_DATE`, `CURRENT_TIME`, `NOW()`
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
- **메모리 상한**: `Options.CacheSize`(기본 32 MiB, 페이지·노드 캐시), `Options.SortMemory`(기본 64 MiB, 초과 시 `Options.TempDir`에 임시 파일).
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

- 트리거, 윈도우 함수, JSON 함수 미지원
- 조인 순서는 FROM에 쓴 순서를 따름 (통계 기반 재배치·covering index 없음) → `EXPLAIN`으로 확인
- 외래 키는 항상 적용되며 즉시 검사 (지연 제약·`SET DEFAULT` 없음). 부모 테이블을 먼저 만들어야 함
- 뷰는 읽기 전용이며 조회할 때마다 실행됨
- 집합 연산(`UNION` 등)과 해시 조인의 빌드 측은 메모리에서 처리
- 열린 `Rows`·긴 조회는 그 시점 이후의 체크포인트(와 `VACUUM` 교체)를 막아 WAL이 커질 수 있음 → 다 읽은 `Rows`는 닫기
- 하나의 쓰기 트랜잭션에서 변경한 페이지는 커밋 전까지 메모리에 유지됨
- 파일 포맷 버전 3. 개발 중 버전(1, 2)으로 만든 파일은 열 수 없음

## 문서

- [보완 검토와 개발 계획 (ROADMAP)](docs/ROADMAP.md)
- [아키텍처](docs/ARCHITECTURE.md)
- [백업과 미러링](docs/REPLICATION.md)
- [시계열·인메모리 DB 확장 적합성 검토](docs/REVIEW-timeseries-inmemory.md)

## 개발

```bash
go test ./...           # 단위/통합 테스트
go test -race ./...     # 레이스 검사
BANGDB_TEST_STORE=reversed go test .   # 저장 방식 인터페이스 우회 검사
BANGDB_TEST_STORE=memory go test .     # 모든 테이블을 인메모리로 실행
go test -bench . -run XXX .
```
