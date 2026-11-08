# 변경 이력

버전은 `주.부.커밋수`입니다(`VERSION` + 커밋 수, `bangdb -version`). 호환성 약속은 [docs/COMPATIBILITY.md](docs/COMPATIBILITY.md).

## 0.7 — 실 프로젝트 적용 보완 (2026-11-03)

- **모듈 경로 변경(저장소 이전)**: `github.com/knoxxr/bangdb` → `github.com/minaryhub/bangdb`. 가져오기(import)·`go get`·`go install` 주소를 새 경로로 바꿔야 합니다(예: `import "github.com/minaryhub/bangdb"`, 드라이버 `_ "github.com/minaryhub/bangdb/driver"`). 이전 버전은 옛 경로 그대로 받을 수 있습니다. 안드로이드 Java/Kotlin 패키지 `com.knoxxr.bangdb`는 그대로입니다
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
