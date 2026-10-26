# Test Stratejisi

Projede beş test katmanı var:

1. Birim testleri
2. PostgreSQL'e karşı entegrasyon testleri (Testcontainers ile)
3. Gerçek sunucuya karşı kara kutu doğrulama (`make verify`)
4. Yük altında uçtan uca simülasyon
5. Kontrol deneyleri

Kontrol deneyleri şu soruyu cevaplıyor: test gerçekten bir şey kanıtlıyor mu? Bunun için koruma mekanizması kasıtlı olarak kaldırılıyor ve aynı testin bu kez başarısız olduğu gösteriliyor.

## Çalıştırma

| Komut | Ne gerekir | Ne çalışır |
|---|---|---|
| `make test-unit` | Go, Node | Auth, HTTP yardımcıları, frontend reducer |
| `make test-integration` | Go, Docker | Tüm entegrasyon testleri. Her test paketi kendi `postgres:16-alpine` container'ını başlatır. Docker yoksa **başarısız olur** (`TESTCONTAINERS_REQUIRED=1`). |
| `cd backend && go test ./...` | Go | Docker varsa her şeyi çalıştırır; yoksa entegrasyon testlerini atlar |
| `TEST_DATABASE_URL=… go test -p 1 ./...` | Go, boş bir veritabanı | Container yerine verilen veritabanını kullanır. Testler tabloları sildiği için veritabanı **atılabilir** olmalı; paketler veritabanını paylaştığı için `-p 1` şart. |
| `make verify` | Docker | Stack'i ayağa kaldırır ve F-1…F-6, T-1…T-4 tablosunu basar |
| `make simulate DURATION=2m` | Docker | 50 şubeye trafik gönderir. 5xx, ağ hatası veya idempotency uyumsuzluğu olursa hata koduyla çıkar. |

## Test envanteri

**Toplam:** 35 Go testi ve 4 frontend testi. Bunlara ek olarak `make verify` 10 gereksinim altında yaklaşık 130 kontrol yapıyor.

### Birim testleri

| Paket | Test | Kanıtladığı |
|---|---|---|
| `auth` | `TestPasswordRoundTrip`, `TestPasswordNeedsRehash`, `TestVerifyRejectsMalformedHash` | Argon2id PHC formatı, yanlış parolanın reddi, zayıf parametreli hash'lerin yükseltme için işaretlenmesi, bozuk hash'lerin reddi |
| `auth` | `TestJWTRoundTrip`, `TestJWTRejections` | Başka anahtarla imzalanmış, süresi dolmuş, yanlış issuer'lı, `alg: none` ile gelen veya bilinmeyen rollü token'ların reddi |
| `auth` | `TestRBAC`, `TestMiddlewareChain` | Rol → izin matrisi; 401 ve 403 ayrımı |
| `auth` | `TestNormalizeEmail` | E-posta normalleştirme ve doğrulama |
| `httpx` | `TestDDOMarkOnEveryResponse` | Başlığın 200, 500 ve 404 yanıtlarında bulunması |
| `queue` | `TestValidSlot` | Randevu slot kuralları |
| frontend | `boardReducer.test.js` (4 test) | Snapshot'ın durumu tamamen değiştirmesi; `id ≤ lastEventId` olan olayların yok sayılması (tekrar oynatma ile canlı akışın çakışması); biten biletlerin ekrandan düşmesi |

### Entegrasyon testleri: sıra motoru (`internal/queue`)

| Test | Kanıtladığı |
|---|---|
| `TestCallNextNoRaceUnderConcurrency` | 500 bilet, 10 gişe, 50 eşzamanlı çağırıcı. Her bilet tam bir kez çağrılıyor ve çağırıcıya söylenen gişe veritabanındakiyle aynı. |
| `TestNaiveCallNextRaces` | **Kontrol testi:** Aynı test düzeni kilitsiz `SELECT`+`UPDATE` ile. Testin yarışı yakalayabildiğini gösteriyor (aşağıdaki sonuçlara bakın). |
| `TestCallNextSkipsLockedRows` | Başka bir transaction 1 numaralı bileti kilitli tutarken `CallNext` beklemeden 2 numarayı döndürüyor. |
| `TestCreateTicketConcurrentNumbering` | 100 eşzamanlı istek tam olarak 1…100 numaralarını üretiyor: boşluk yok, tekrar yok. |
| `TestCreateTicketIdempotencyConcurrent` | Aynı anahtarla 25 eşzamanlı istek: tek bilet ve 25 bayt bayt aynı yanıt (24'ü tekrar oynatma). Aynı anahtar farklı gövdeyle 422, anahtar yoksa 400. Başka bir kullanıcı aynı anahtarı çakışmadan kullanabiliyor. |
| `TestWaitTimeStats` | 10–100 saniyelik bekleme sürelerinde median = 55 ve P90 = 91, elle hesaplanan değerlerle aynı. Pencere dışındaki aykırı değer dışarıda kalıyor; veri olmayan bir hizmette sonuç `null`. |
| `TestAppointmentConcurrentCheckInIssuesOneTicket` | Aynı randevuya 20 eşzamanlı check-in: tek bilet. Çift rezervasyon reddediliyor, zamanı gelmemiş check-in reddediliyor, olaylar doğru sırada yazılıyor. |

### Entegrasyon testleri: gerçek zamanlı akış (`internal/realtime`)

| Test | Kanıtladığı |
|---|---|
| `TestHubDeliversEveryEventInOrderUnderConcurrency` | 30 eşzamanlı yazıcı varken abone, tablodaki olayların **tamamını** tablodaki sırayla alıyor. Recovery'nin dayandığı özellik bu. |
| `TestWebSocketRecoversMissedEvents` | Önce snapshot, sonra canlı olaylar. Bağlantı kopuyor, çevrimdışıyken 5 olay oluyor, yeniden bağlanınca **tam olarak** bu 5 olay geliyor, ardından `replay_complete` ve canlı akış devam ediyor. |
| `TestWebSocketFallsBackToSnapshot` | İstemcinin son olayı silinmişse tekrar oynatma yerine snapshot gönderiliyor. |
| `TestWebSocketAuth` | Geçersiz token → `4001`; yetkisiz rol → `4003`; bağlantı sırasında süresi dolan token → `4001`. |

### Entegrasyon testleri: uçtan uca HTTP (`internal/server`)

Bu testler `main` ile aynı router, middleware zinciri ve arka plan işçileri üzerinden çalışıyor.

| # | Test | Kanıtladığı |
|---|---|---|
| 1 | `TestRegisterLoginMeUsesArgon2id` | Kayıt, giriş ve `/me`; veritabanında `$argon2id$v=19$` ile başlayan hash; bilinmeyen kullanıcıya 401 |
| 2 | `TestRefreshRotationAndReuseDetection` | Rotasyon; eski token'ın tekrar kullanımı → 401 ve yeni token da iptal; tekrar kullanımın audit kaydı |
| 3 | `TestRBACAcrossRoles` | 9 hücreli rol × uç nokta matrisi (anonim, `user`, `display`, `staff`, `admin`) |
| 4 | `TestDDOMarkHeaderOnEveryResponse` | Başlığın 200, 400, 401, 404, 405 ve **101 (WebSocket upgrade)** yanıtlarında bulunması |
| 5 | `TestHealthAndHiddenDiagnostics` | `/health`; `/_hz/bfe1` DB'nin erişilebilir olduğunu ve şema sürümünün 7 olduğunu raporluyor |
| 6 | `TestAuditLogIsAppendOnly` | Girişin audit'e yazılması; UPDATE, DELETE ve TRUNCATE'in reddedilmesi |
| 7 | `TestTicketIdempotencyOverHTTP` | Bayt bayt aynı tekrar oynatma + `Idempotent-Replayed` başlığı; 422 ve 400 durumları; tek bilet |
| 8 | `TestCallNextAndFinishLifecycle` | FIFO; boş kuyrukta 204; bitirme 200, ikinci bitirme 409, geçersiz sonuç 400; başka şubenin gişesi 404 |
| 9 | `TestConcurrentCallNextOverHTTPNeverDoubleAssigns` | HTTP üzerinden 40 bilet ve 80 eşzamanlı çağırıcı: 40 farklı bilet, 40 boş yanıt |
| 10 | `TestWaitTimeStatsOverHTTP` | 5 örnek, P90 ≥ median; geçersiz pencere değeri 400 |
| 11 | `TestAppointmentBookingAndCheckIn` | Rezervasyon, çift rezervasyon 409, hatalı slot 400, başkasının randevusu 404, check-in ile bilet üretilmesi, ikinci check-in 409 |
| 12 | `TestWebSocketRecoveryThroughFullRouter` | Recovery'nin chi'nin Logger sarmalayıcısı ve gerçek sunucu zaman aşımlarıyla da çalışması |
| 13 | `TestMigrationsRoundTripDownAndUp` | Boş bir veritabanında migration'ları yukarı, sıfıra kadar aşağı ve tekrar yukarı çalıştırma. **Down migration'ları test eden tek yer.** |
| 14 | `TestSchemaNamingConvention` | `public` şemasındaki her tablonun `qa-` önekini taşıması; `qa-ev-users` ve `qa-audit_logs` tablolarının var olması |

### Kara kutu doğrulama: `make verify`

`simulator verify` çalışan bir deployment'a yalnızca dışarıdan, HTTP ve WebSocket üzerinden erişiyor. Her gereksinim için kontroller çalıştırıp şu tabloyu basıyor:

```
ID   Gereksinim                                               Sonuç  Süre   Ayrıntı
F-1  Kimlik doğrulama (Argon2id + JWT, refresh rotasyonu)     PASS   58ms   12 assertions
F-2  RBAC: rol bazlı yetkilendirme                            PASS   50ms   9 assertions
F-3  Bilet oluşturma + Idempotency-Key                        PASS   9ms    7 assertions
F-4  Sıra çağırma motoru (FIFO, call-next, finish)            PASS   32ms   10 assertions
F-5  Bekleme süresi tahmini (Median / P90)                    PASS   1ms    4 assertions
F-6  Randevu (rezervasyon, çakışma, check-in)                 PASS   9ms    9 assertions
T-1  X-DDO-Mark: bfe1f2 başlığı tüm yanıtlarda                PASS   2ms    8 assertions
T-2  /health ve gizli /_hz/bfe1 uçları                        PASS   1ms    4 assertions
T-3  WebSocket canlı akış + kopma sonrası recovery            PASS   30ms   11 assertions
T-4  Race condition yok (SKIP LOCKED, eşzamanlı idempotency)  PASS   382ms  54 assertions

10/10 PASS
```

- **Çakışma yok:** Her çalıştırma kendine ait bir `VF-xxxxxx` şubesi ve test kullanıcıları oluşturuyor. Bu yüzden tekrar tekrar ve seed verisinin yanında çalıştırılabiliyor.
- **Hata durumu:** Bir kontrol başarısız olursa çıkış kodu 1 oluyor. Bu, yanlış admin parolasıyla denendi: 0/10 FAIL basıldı ve araç 1 koduyla çıktı.

Gereksinim ID'leri ile kontroller arasındaki eşleme, [simulator/verify.go](simulator/verify.go) dosyasının başındaki `checks` listesinde.

## Kontrol deneyleri ve ölçümler

Bu sonuçlar geliştirme sırasında elde edildi. Tekrarlanabilmeleri için nasıl yapıldıkları da yazılı.

| Deney | Sonuç |
|---|---|
| Kilitsiz `SELECT`+`UPDATE` ile sıra çağırma (`TestNaiveCallNextRaces`) | 300 biletin 223–232'si birden fazla gişeye verildi (birkaç çalıştırma). `SKIP LOCKED` sürümü 20 tekrarın 20'sinde temiz. |
| `qa_emit_event`'ten advisory lock kaldırıldı ve id ayırma ile commit arasına küçük bir rastgele gecikme eklendi; `TestHubDeliversEveryEventInOrderUnderConcurrency` 5 kez çalıştırıldı | 5 çalıştırmanın 4'ü başarısız, olaylar **kalıcı olarak kayboldu** (ör. "received 308 of 311"). Kilitli sürüm 10 tekrarın 10'unda geçti. |
| Idempotent yanıt `jsonb` kolonunda saklandı | Tekrar oynatılan yanıt bayt bayt farklı çıktı (anahtar sırası değişti), test yakaladı. Kolon `bytea` yapıldı. |
| Görünen bilet kodu SQL `lpad` ile üretildi | Parametre tipi çakışması bilet oluşturmayı tamamen kırdı ve 999'dan sonraki numaralar kesilecekti; test yakaladı. Kod artık Go'da `%03d` ile üretiliyor. |
| Uçtan uca: gerçek sunucu, 75 saniye, saniyede 40 bilet + 10 randevu + 30 çağırma | 8.114 istek: 0 adet 5xx, 0 ağ hatası, **394/394** idempotency tekrarı doğru. Bilet oluşturmada p50 3,0 ms, p99 54 ms. |
| Uçtan uca: aynı yük altında bir ekran 45 saniye bağlı kaldı, 7 saniye koptu, sonra `last_event_id` ile geri döndü | Bağlantı sunucunun 35 saniyelik `WriteTimeout`'unu aşıp ayakta kaldı. Geri dönüşte 7 olay kurtarıldı. Ekranın aldığı **131 olay, veritabanındaki SB-01 olay dizisiyle birebir aynı** (`diff` ile karşılaştırıldı). |
| `simulator seed` iki kez art arda | İkinci çalıştırma hiçbir şey oluşturmadı. `created_at`'e göre ilk kullanıcı `seed.dalga@ddo-case.invalid` (admin). |

## Test edilmeyenler

Neyin test **edilmediğini** bilmek, neyin test edildiğini bilmek kadar önemli:

- **Docker imajları ve compose bu ortamda hiç çalıştırılmadı.** Proje Docker'sız bir makinede geliştirildi. `docker compose up`, multi-stage build'ler, nginx proxy'si (`/ws` upgrade dahil), `make` hedefleri ve testcontainers'ın container başlatması gerçek Docker'da denenmedi.
  - **Ayrı ayrı doğrulananlar:** Compose dosyası YAML olarak doğru okunuyor; servis adları ve `depends_on` zinciri doğru. `api healthcheck` alt komutu sağlıklı sunucuda 0, kapalı portta 1 döndürüyor.
  - **Testcontainers testlerinin içeriği:** `TEST_DATABASE_URL` ile yerel PostgreSQL 16.4'e karşı çalıştırıldı (35/35 geçti). Docker'sız ortamda atlandıkları da doğrulandı.
- **Go `-race` dedektörü çalıştırılmadı.** cgo/gcc gerektiriyor. Kanıtlanan yarışlar veritabanı düzeyinde ve doğrudan ölçülüyor; Go bellek yarışları için CI'da `go test -race` eklenmeli.
- **Tarayıcıda UI testi yapılmadı.** React ekranı derleniyor ve reducer testleri geçiyor. Bağlantı ve recovery davranışı tarayıcıda değil, Node'un WebSocket'iyle ve Go istemcisiyle denendi.
- **Bazı akışların testi yok:** Yavaş istemcinin `1013` ile düşürülmesi, retention işinin (janitor) zamanlaması ve birden fazla API instance'ının birlikte çalışması.
- **Yük testi sınırlı kaldı.** Yük tek makinede yaklaşık 110 işlem/saniye ile sınırlı tutuldu; kapasite sınırı ölçülmedi.

## CI önerisi

```bash
cd backend  && TESTCONTAINERS_REQUIRED=1 go test -race -count=1 ./...
cd frontend && npm ci && npm test && npm run build
make verify
```
