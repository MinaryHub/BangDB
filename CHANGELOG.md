# 변경 이력

버전은 `주.부.커밋수`입니다(`VERSION` + 커밋 수, `bangdb -version`). 호환성 약속은 [docs/COMPATIBILITY.md](docs/COMPATIBILITY.md).

## 다음 릴리스 — SQL 완성도, 셸

- **CTE**: `WITH name [(cols)] AS (…)`와 **`WITH RECURSIVE`** — `SELECT`/`INSERT`/`UPDATE`/`DELETE` 앞, 서브쿼리·파생 테이블·뷰 안. 재귀는 SQLite 방식(큐에서 행을 꺼내 재귀 SELECT에 넣음): `UNION`은 중복 제거, 본문의 `LIMIT`/`OFFSET`은 결과 제한, `ORDER BY`는 처리 순서(깊이 우선·너비 우선)
- **식 인덱스**: `CREATE [UNIQUE] INDEX … ON t (lower(email), …) [WHERE …]`. 조건·`ORDER BY`가 인덱스의 식과 같으면 플래너가 사용. 타입을 아는 식은 범위·정렬에도, 그 밖의 식은 동등·`IN`에 사용. `PRAGMA index_info`는 식 부분을 SQLite처럼 `cid = -2`로 표시
- **생성 컬럼**: `c TYPE [GENERATED ALWAYS] AS (식) [STORED|VIRTUAL]` (쓸 때 계산해 저장). 덤프는 생성 컬럼 값을 빼고 내보냄
- **테이블 값 함수**: `json_each`, `json_tree`(SQLite와 같은 컬럼), `generate_series` — 왼쪽 테이블의 컬럼을 인수로 받는 lateral 조인
- `PRAGMA schema [(이름)]`: 테이블·인덱스·뷰·트리거의 CREATE 문 (`type`, `name`, `tbl_name`, `sql`)
- **CLI 셸**: 줄 편집(방향키, Home/End, Ctrl-A/E/K/U/W), 히스토리(↑/↓, `~/.bangdb_history`, `-history`), Tab 자동 완성(테이블·컬럼·키워드·점 명령) — 외부 의존성 없이 Linux·macOS·FreeBSD·Windows 콘솔 지원. `.mode table|list|csv|json|line`, `.headers on|off`, `.output FILE`. 명령줄 인수로 점 명령 실행(`bangdb app.db ".mode csv" "SELECT …"`). 원격 셸에서 `.tables`·`.schema` 사용 가능. 표 출력이 전각 문자(한글 등) 폭을 맞춤
- 카탈로그에 필드 추가(`Index.exprs`, `Column.generated`): 이 기능을 쓴 파일은 이전 버전에서 열 수 없음(되돌리기는 원래 미지원). 기능을 쓰지 않은 파일의 형식은 그대로

## 0.7 — 실 프로젝트 적용 보완 (2026-11-03)

- **BangDB Manager 연결**: HTTP API에 `/v1/*`(health·tables·query·stats) 추가 — Manager의 "BangDB (Go)" 연결 종류가 그대로 동작. Basic 인증은 아무 사용자 + `-password` 값도 허용, `/v1/stats`에 API 요청·인증 거부 수
- CLI: Termux(안드로이드)에서 모든 명령이 "database file is corrupt"로 실패하던 문제 수정(링커가 넘기는 실행 파일 경로를 DB로 오인). 열기 오류에 파일 경로 표시, DB가 아닌 파일을 열 때 `-wal`을 만들지 않음
- **안드로이드 앱 내장**: `mobile/` 래퍼(gomobile) → `.aar`, Kotlin/Java `com.knoxxr.bangdb` (`Database`, `Tx`, `Rows`, `Args`, `Migrations`). CI가 `.aar`를 만들고 예제를 컴파일. `scripts/build.sh aar`, `all`에 android/arm64 CLI 추가 (docs/ANDROID.md)
- Windows 파일 잠금(`LockFileEx`): 두 프로세스가 같은 파일을 여는 것을 막음. 모든 OS에서 두 번째 열기는 `ErrLocked`
- 3개 OS CI (테스트·경합 검사·크래시 토처·퍼징·빌드)
- CLI 설정 파일·`BANGDB_*` 환경 변수·키/비밀번호 파일, `-print-config`
- 서버 모드 기본 쿼리 타임아웃 60초, HTTP 타임아웃·동시 실행 제한, 비밀번호 없는 서버 경고
- `PRAGMA` (`user_version`, `table_info`, `index_list`, `index_info`, `table_list`, `integrity_check`, `foreign_keys`, `journal_mode`, `page_count`, `page_size`), `DB.Migrate`
- 파일 포맷 호환 픽스처 테스트, 호환성 정책 문서
- **정확한 `DECIMAL(p,s)`/`NUMERIC(p,s)`** (최대 18자리, 반올림·자릿수 검사, 정확한 합계·평균, 인덱스 지원), `CAST(x AS DECIMAL(p,s))`
- 계정·역할(read/write/admin, PBKDF2 해시 계정 파일, `-add-user`)을 서버·HTTP에 적용
- `/metrics`(Prometheus), 구조화 로그(`log_format`, `log_level`), 웹 콘솔(`/`)
- 정기 백업 + 백업 검증(`StartBackups`, `VerifyBackup`, `backup_dir`/`backup_interval`/`backup_keep`)
- 트리거(BEFORE/AFTER, INSERT/UPDATE OF/DELETE, WHEN, NEW/OLD, RAISE), 덤프·SQLite 덤프 가져오기에 포함
- 부분 인덱스 `CREATE [UNIQUE] INDEX ... WHERE 조건` (예: 삭제되지 않은 행만 이메일 유일). 질의 조건이 인덱스 조건을 포함할 때만 플래너가 사용
- 복제본이 받은 프레임을 묶어 fsync 1회로 적용(느린 디스크에서 지연 감소)
- **메모리보다 큰 트랜잭션**: 쓰기 트랜잭션이 바꾼 페이지가 `Options.TxMemory`(CLI `tx_memory`, 기본 64 MiB)를 넘으면 임시 파일로 옮기고 커밋 때 WAL로 스트리밍. 이전에는 1천만 행 표에서 `DELETE` 10%(100만 행) 한 문장이 메모리 14 GB를 써 OOM으로 종료됐음. `/metrics`에 `tx_spills_total`
- 체크포인트·WAL 복구를 4,096페이지 단위로 스트리밍: 큰 커밋이나 VACUUM 뒤 WAL 전체를 메모리에 올리던 문제 수정(2M 행 VACUUM 최대 RSS 1.7 GB → 0.5 GB). 큰 WAL을 남기고 비정상 종료한 DB도 적은 메모리로 열림
- 1,000만 행 `UPDATE`/`DELETE`가 대상 행 전체를 메모리에 모으지 않음(앞의 4,096행만 보관, 나머지는 rowid로 다시 읽음)
- VACUUM 후에도 누적 통계(커밋·체크포인트·페이지 읽기 수)가 0으로 돌아가지 않음
- 대용량·장시간 시험(`TestScale` 1천만 행, `TestSoak`), 단계별 최대 메모리 보고. 결과는 docs/ROADMAP.md 7절
- Windows: 데이터 밖 1바이트 잠금(다른 프로그램이 파일을 읽을 수 있음), 디렉터리 fsync 생략, 파일 교체 재시도

## 0.6 — 운영·분산

- HTTP/JSON API (`httpapi`, `-http`), 빌드 버전 표기(`scripts/build.sh`, `-version`)
- 서버 모드(`server`/`client`, `bangdb://` DSN), 저장 데이터 암호화, 반동기 복제·펜싱·PITR
- 관측성(`Stats`, 슬로우 쿼리, `EXPLAIN ANALYZE`), 덤프·SQLite 가져오기·CSV
- 윈도우·JSON 함수, `ANALYZE`·조인 순서·covering index, `Prepare`, 시계열 compaction, 메모리 테이블 persist

## 0.5 이전

관계형 코어(WAL·B+트리·SQL·플래너·트랜잭션), 백업·미러링, 저장 엔진 인터페이스, 인메모리 테이블, 시계열 테이블. 자세한 내용은 [docs/ROADMAP.md](docs/ROADMAP.md).
