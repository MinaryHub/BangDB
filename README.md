# BangDB

Go로 작성된 **경량 임베디드 관계형 데이터베이스**입니다. 외부 의존성 0개, 단일 파일 저장, 프로젝트에 라이브러리로 바로 넣어 쓰는 것을 목표로 합니다 (SQLite와 같은 사용 방식).

- 순수 Go, 표준 라이브러리만 사용 (`go.mod`에 require 없음)
- 단일 파일 + WAL(`<파일>-wal`) 기반 **원자적·내구성 있는 커밋과 크래시 복구**
- `:memory:` 인메모리 모드
- SQL: `CREATE/DROP TABLE`, `ALTER TABLE ADD COLUMN`, `CREATE [UNIQUE] INDEX`, `INSERT/SELECT/UPDATE/DELETE`, `JOIN`(INNER/LEFT/CROSS), `GROUP BY/HAVING`, `ORDER BY`, `LIMIT/OFFSET`, `DISTINCT`, `CASE`, `IN`, `BETWEEN`, `LIKE`, 집계/스칼라 함수, `EXPLAIN`
- B+트리 기반 테이블/인덱스, 인덱스를 활용하는 쿼리 플래너 (rowid·인덱스 동등/범위/IN 검색, 조인 시 인덱스 lookup)
- 트랜잭션: `BEGIN/COMMIT/ROLLBACK`, 문장 단위 원자성, **읽기는 쓰기 트랜잭션에 막히지 않음** (커밋된 스냅샷을 읽음)
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
	"fmt"
	"log"

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

	rows, _ := db.Query(`SELECT id, name, age FROM users WHERE age > ? ORDER BY age DESC`, 20)
	for rows.Next() {
		var id int64
		var name string
		var age int
		rows.Scan(&id, &name, &age)
		fmt.Println(id, name, age)
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

### CLI

```bash
go install github.com/knoxxr/bangdb/cmd/bangdb@latest

bangdb app.db                         # 대화형 셸 (.help, .tables, .schema, .timer on)
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
- 스칼라: `LOWER`, `UPPER`, `LENGTH`, `SUBSTR`, `TRIM/LTRIM/RTRIM`, `REPLACE`, `INSTR`, `ABS`, `ROUND`, `COALESCE`, `IFNULL`, `NULLIF`, `MIN/MAX(a, b, ...)`, `TYPEOF`, `CAST(x AS type)`, `CURRENT_TIMESTAMP`, `CURRENT_DATE`, `CURRENT_TIME`, `NOW()`, `UNIXEPOCH()`

## 동시성과 내구성

- 쓰기 트랜잭션은 한 번에 하나입니다. 다른 쓰기는 `BusyTimeout`(기본 5초)까지 기다린 뒤 `ErrBusy`를 반환합니다.
- 읽기는 진행 중인 쓰기 트랜잭션과 무관하게 **마지막으로 커밋된 상태**를 읽습니다. 커밋을 반영하는 짧은 순간에만 대기합니다.
- 커밋 순서: 변경 페이지를 WAL에 기록 → fsync → DB 파일에 반영 → fsync → WAL 비움. 어느 단계에서 프로세스가 죽어도 다음 `Open` 시 커밋된 트랜잭션은 복구되고, 완료되지 않은 트랜잭션은 버려집니다.
- 한 파일은 한 프로세스만 열 수 있습니다 (Unix `flock`).

## 성능 (참고치)

4코어 리눅스 컨테이너, `go test -bench .` 기준:

| 작업 | 시간 |
|------|------|
| 트랜잭션 내 INSERT (인덱스 1개 포함) | ~27 µs/행 |
| 자동 커밋 INSERT (fsync 2회) | ~0.65 ms/행 |
| PK 조회 (10만 행) | ~9 µs |
| 보조 인덱스 조회 (10만 행) | ~13 µs |
| 10만 행 전체 스캔 + GROUP BY | ~34 ms |

대량 삽입은 반드시 트랜잭션으로 묶으세요.

## 현재 제약 사항

- 서브쿼리, `UNION`, 뷰, 외래 키, `CHECK`, 트리거 미지원
- 인덱스와 PRIMARY KEY는 단일 컬럼만 지원
- 삭제 시 B+트리 재균형을 하지 않음 → 삭제로 생긴 빈 공간은 같은 트리에서 재사용되거나 `DROP TABLE` 시 회수 (VACUUM 없음)
- 쿼리 결과는 메모리에 모두 적재된 뒤 반환됨 (스트리밍 커서 아님)
- 하나의 쓰기 트랜잭션에서 변경한 페이지는 커밋 전까지 메모리에 유지됨
- 인덱스 키 최대 약 500바이트 (긴 TEXT 컬럼은 인덱싱 불가)
- 이름 있는 파라미터(`:name`) 미지원, `?`만 사용

## 문서

- [아키텍처](docs/ARCHITECTURE.md)
- [시계열·인메모리 DB 확장 적합성 검토](docs/REVIEW-timeseries-inmemory.md)

## 개발

```bash
go test ./...           # 단위/통합 테스트
go test -race ./...     # 레이스 검사
go test -bench . -run XXX .
```
