# 변경 이력

버전은 `주.부.커밋수`입니다(`VERSION` + 커밋 수, `bangdb -version`). 호환성 약속은 [docs/COMPATIBILITY.md](docs/COMPATIBILITY.md).

## 0.7 (진행 중) — 실 프로젝트 적용 보완

- Windows 파일 잠금(`LockFileEx`): 두 프로세스가 같은 파일을 여는 것을 막음. 모든 OS에서 두 번째 열기는 `ErrLocked`
- 3개 OS CI (테스트·경합 검사·크래시 토처·퍼징·빌드)
- CLI 설정 파일·`BANGDB_*` 환경 변수·키/비밀번호 파일, `-print-config`
- 서버 모드 기본 쿼리 타임아웃 60초, HTTP 타임아웃·동시 실행 제한, 비밀번호 없는 서버 경고
- `PRAGMA` (`user_version`, `table_info`, `index_list`, `index_info`, `table_list`, `integrity_check`, `foreign_keys`, `journal_mode`, `page_count`, `page_size`), `DB.Migrate`
- 파일 포맷 호환 픽스처 테스트, 호환성 정책 문서

## 0.6 — 운영·분산

- HTTP/JSON API (`httpapi`, `-http`), 빌드 버전 표기(`scripts/build.sh`, `-version`)
- 서버 모드(`server`/`client`, `bangdb://` DSN), 저장 데이터 암호화, 반동기 복제·펜싱·PITR
- 관측성(`Stats`, 슬로우 쿼리, `EXPLAIN ANALYZE`), 덤프·SQLite 가져오기·CSV
- 윈도우·JSON 함수, `ANALYZE`·조인 순서·covering index, `Prepare`, 시계열 compaction, 메모리 테이블 persist

## 0.5 이전

관계형 코어(WAL·B+트리·SQL·플래너·트랜잭션), 백업·미러링, 저장 엔진 인터페이스, 인메모리 테이블, 시계열 테이블. 자세한 내용은 [docs/ROADMAP.md](docs/ROADMAP.md).
