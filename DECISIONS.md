# Tasarım Kararları

Her karar için ne seçildiği, neden seçildiği, bedeli ve değerlendirilen alternatifler yazılı. Kararlar konu sırasıyla listelendi.

## 1. Router: Chi (Fiber değil)

- **Karar:** `go-chi/chi/v5`.
- **Neden:** Chi, `net/http` ile birebir uyumlu. `http.Handler` middleware'leri, `httptest`, `http.ResponseController` (WebSocket bağlantılarında zaman aşımlarını kaldırmak için) ve `coder/websocket` olduğu gibi çalışıyor. Fiber `fasthttp` üzerine kurulu; standart ekosistemle arasına bir adaptör katmanı giriyor.
- **Bedel:** Fiber'in ham hız avantajından vazgeçildi. Bu iş yükünde darboğaz veritabanı, router değil.

## 2. Veritabanı erişimi: `database/sql` + pgx, ORM yok

- **Neden:** Kritik bölümler (`FOR UPDATE SKIP LOCKED`, `ON CONFLICT`, advisory lock, `percentile_cont`) zaten elle yazılmış SQL. Bir ORM bunları gizlerdi. `database/sql`, goose ile aynı bağlantı havuzunu paylaşıyor. Sadece `LISTEN` için doğrudan pgx bağlantısı kullanılıyor.
- **Bedel:** Satırları struct'a aktarmak elle yapılıyor.

## 3. Migration: goose, SQL dosyaları binary'ye gömülü, açılışta uygulanıyor

- **Neden:** Tek binary ile dağıtım yapılıyor; ayrı bir migration adımı unutulamaz.
- **Önek kuralı:** goose'un sürüm tablosu da `"qa-goose_db_version"` adını alıyor. goose tablo adını SQL'e olduğu gibi yerleştirdiği ve adda tire olduğu için ad önceden tırnaklanmış olarak veriliyor.
- **Bedel:** Birden fazla instance aynı anda açılırsa migration'ları ikisi birden uygulamaya çalışabilir. Production'da migration'ları ayrı bir iş (job) olarak çalıştırmak daha güvenli.

## 4. Tablo adlarında `qa-` öneki

- Gereksinim gereği. Tire içerdiği için her sorguda çift tırnak gerekiyor.
- Bir entegrasyon testi `public` şemasındaki **her** tablonun bu öneki taşıdığını denetliyor. Böylece yeni bir migration kuralı yanlışlıkla bozamıyor.

## 5. Parola hash'leme: Argon2id, OWASP'ın asgari önerisi (19 MiB, t=2, p=1)

- **Neden:** Bellek maliyeti brute-force'u pahalılaştırıyor. RFC 9106'nın önerdiği 64 MiB yerine 19 MiB seçildi; böylece eşzamanlı girişlerde bellek tüketimi öngörülebilir kalıyor (32 eşzamanlı giriş ≈ 600 MiB).
- **Parametre yükseltme:** Parametreler hash ile birlikte (PHC formatında) saklanıyor. İleride yükseltilirse eski hash'ler kullanıcı giriş yaptığında otomatik yeniden hash'leniyor.
- **Uzunluk sınırı:** Parola en fazla 128 bayt; hash maliyetini sınırlamak için.

## 6. Token'lar: kısa ömürlü HS256 JWT + opak, rotasyonlu refresh token

- **Access token:** 15 dakika. Doğrulama için veritabanına gitmek gerekmiyor.
- **Refresh token:** 32 bayt rastgele değer; veritabanında yalnızca SHA-256 hash'i saklanıyor.
- **Rotasyon ve tekrar kullanım:** Her kullanımda yenileniyor. İptal edilmiş bir token tekrar gelirse çalınmış sayılıyor ve kullanıcının tüm oturumları kapatılıyor.
- **Neden HS256, RS256 değil:** Token'ları tek bir servis hem üretiyor hem doğruluyor; açık anahtar dağıtmaya gerek yok. Başka servisler token doğrulayacaksa RS256/EdDSA'ya geçilmeli.
- **Bedel:** Rol değişikliği, mevcut access token'ın süresi dolana kadar (en fazla 15 dakika) gecikebiliyor. Bu süre, rol değişince oturumları kapatarak kısaltılıyor.
- **İstemci tarafı yan etki:** Rotasyon yüzünden eşzamanlı iki refresh isteği tekrar kullanım gibi görünür. Frontend bu yüzden aynı anda tek bir refresh isteği gönderiyor. Simülatör ise refresh yerine yeniden giriş yapıyor.

## 7. RBAC: rol → izin eşlemesi kodda

- **Neden:** Dört sabit rol var. İzinleri veritabanında tutmak hem karmaşıklık hem bir sorgu maliyeti getirirdi. Rotalar rol yerine izin istiyor (`RequirePermission`); bu sayede yeni bir rol eklemek tek bir satır.
- **Ne zaman değişmeli:** Kuruma özel, çalışma anında değiştirilebilen roller gerekirse eşleme veritabanına taşınmalı.
- **Rol listesi:** `display` rolü lobi ekranları için ayrıca eklendi. Ekran hesabının ele geçirilmesi, yalnızca salt-okunur kuyruk erişimi demek.

## 8. Sıra çağırma: tek ifadede `FOR UPDATE SKIP LOCKED`

- **Neden:** Uygulama içi mutex birden fazla instance arasında çalışmaz. Düz `FOR UPDATE` doğru sonuç verir ama tüm çağırıcıları aynı satırda sıraya dizer. `SKIP LOCKED` ise her çağırıcıya kilitsiz bir sonraki satırı veriyor: doğru ve beklemesiz. `UPDATE` içindeki `status='waiting'` kontrolü ek bir güvence.
- **Kanıt:** Kilitsiz `SELECT`+`UPDATE` ile karşılaştıran bir kontrol testi var. Kilitsiz hâlde 300 biletin ~225'i birden fazla gişeye verildi; bkz. [TESTING.md](TESTING.md).
- **Bedel:** `SKIP LOCKED` kesin FIFO garantisi vermiyor. Aynı anda çağrılan iki biletten küçük numaralı olan, diğerinden birkaç milisaniye sonra kaydedilebilir. Gişe ekranı açısından bu fark görünmez.

## 9. Bilet numarası: hizmet ve gün başına sayaç satırı (upsert)

- **Neden:** PostgreSQL `SEQUENCE`'ları rollback'te boşluk bırakır ve her gün sıfırlamak zordur. `INSERT … ON CONFLICT DO UPDATE … RETURNING` hem atomik, hem rollback'te geri alınıyor, hem de gün değişince kendiliğinden sıfırlanıyor.
- **Bedel:** Aynı hizmete gelen bilet oluşturma istekleri, transaction'ın geri kalanı boyunca o sayaç satırında sıraya giriyor. Transaction'lar bu yüzden kısa tutuldu.
- **Gün sınırı:** `QUEUE_TIMEZONE` ile belirleniyor (varsayılan Europe/Istanbul). Sunucunun UTC'de çalışması gün sınırını kaydırmıyor.

## 10. Idempotency: anahtar ve yanıt, işi yapan transaction'ın içinde

- **Neden:** Klasik "önce `in_progress` kaydı yaz, işi yap, sonra yanıtı yaz" yaklaşımında çökmeler yarım kalmış kayıtlar bırakır ve ayrıca bir `409 in progress` durumu gerekir. Burada anahtar alma, iş ve yanıt aynı transaction'da. Aynı anahtarla gelen ikinci istek primary key kilidinde bekleyip kaydedilmiş yanıtı alıyor; işlem rollback olursa anahtar boşa çıkıyor.
- **Yanıt `bytea` olarak saklanıyor, `jsonb` değil:** `jsonb` anahtar sırasını değiştiriyor ve tekrar oynatılan yanıt bayt bayt aynı çıkmıyordu. Bu sorunu test yakaladı.
- **Kapsam:** Anahtarlar kullanıcı ve uç nokta başına ayrı. Bir istemcinin anahtarı başka birinin yanıtını ne ezebiliyor ne de ortaya çıkarabiliyor.
- **Neyin hash'i:** Metot, yol ve sıkıştırılmış JSON gövdesi hash'leniyor. Böylece aynı anahtarla farklı içerik gönderilirse 422 dönüyor.
- **Bedel:** Bilet oluşturma uçları idempotency tablosuna bağlı. Anahtarı kendisi saklayan genel bir middleware yerine, işi yapan handler bunu açıkça kullanıyor. Bu bilinçli bir seçim: genel bir middleware, işin kendisiyle aynı transaction'da çalışamaz.

## 11. Gerçek zamanlı akış: olay tablosu, şube başına advisory lock ve tablodan okuyan hub

- **Olay günlüğü (outbox):** Değişiklikler olayıyla birlikte aynı transaction'da yazılıyor. Böylece "veri değişti ama olay kayboldu" ya da tersi bir durum oluşamıyor.
- **Sıra garantisi:** Recovery'nin doğru olması için bir şube içinde olay ID'lerinin commit sırasıyla görünür olması şart. `qa_emit_event`, şube başına bir `pg_advisory_xact_lock` alıyor: farklı şubeler paralel çalışıyor, aynı şube kendi içinde sıralı.
  - **Kanıt:** Kilit kaldırılınca olaylar kalıcı olarak kayboldu.
  - **Değerlendirilen alternatifler:** Global bir kilit (gereksiz yere tüm şubeleri sıraya dizerdi). `xid8`/`pg_snapshot_xmin` ile sıralama (doğru ama istemciye bileşik bir cursor taşıtırdı).
- **Hub:** `LISTEN/NOTIFY` sadece uyandırma sinyali. Hub olayları her zaman tablodan, şube başına bir cursor ile okuyor; ayrıca 2 saniyede bir yokluyor. Bu sayede birden fazla instance doğru çalışıyor ve kaçan bir bildirim veri kaybına yol açmıyor.
- **Anlık görüntü:** `REPEATABLE READ` ile tek bir snapshot'ta hem son olay ID'si hem bilet listesi okunuyor. Bu ikisi tutarlı olduğu için, snapshot'ın üstüne o ID'den sonraki olayları uygulamak tam olarak güncel durumu veriyor.
- **Recovery mi, snapshot mı:** İstemcinin son gördüğü olay hâlâ tabloda duruyorsa ve geride en fazla 5.000 olay varsa tekrar oynatılıyor; aksi hâlde snapshot gönderiliyor. Saklama süresi (retention) en eski olaylardan başlayarak siliyor; yani son olay duruyorsa ondan sonraki olayların hepsi de duruyor demek.
- **Yavaş istemci:** Her abonenin 1.024 olaylık bir tamponu var. Tampon dolarsa bağlantı `1013` koduyla kapatılıyor ve istemci recovery ile yeniden bağlanıyor. Sınırsız bir kuyruk yerine bu seçildi.
- **Gizlilik:** Olaylar müşteri kimliği içermiyor; lobi ekranına gitmeleri güvenli.

## 12. WebSocket kimlik doğrulaması: token ilk mesajda

- **Neden:** Tarayıcıların WebSocket API'si özel başlık gönderemiyor. Token query string'e konsaydı erişim loglarına ve tarayıcı geçmişine düşerdi.
- **Kurallar:** İlk mesaj 5 saniye içinde gelmezse bağlantı `4408` ile kapatılıyor. Token'ın süresi bağlantı sırasında dolarsa `4001` ile kapatılıyor ve istemci token'ı yenileyip olay kaybetmeden geri dönüyor.
- **Origin:** Çapraz kaynaklı bağlantılar `ALLOWED_ORIGINS` listesiyle sınırlı.
- **Zaman aşımları:** Global 30 saniyelik istek zaman aşımı yalnızca HTTP rotalarına uygulanıyor; WebSocket'lere uygulanırsa bağlantıları öldürürdü. Sunucunun okuma/yazma zaman aşımları da hijack'ten önce sıfırlanıyor.

## 13. Bekleme tahmini: geçmiş beklemelerin median ve P90'ı

- **Neden:** Basit, açıklanabilir ve doğrudan SQL ile hesaplanıyor (`percentile_cont`). P90, "çoğu kişi en fazla bu kadar bekledi" gibi dürüst bir üst sınır veriyor.
- **Bedel:** Kuyruk uzunluğu ve açık gişe sayısı hesaba katılmıyor. Daha iyi bir model, sıradaki konumu gişe başına median servis süresiyle çarpardı. Bu, ileride yapılacak iş olarak bırakıldı.

## 14. Konteynerler: multi-stage build ve distroless

- **Neden:** Çalışma imajında shell, paket yöneticisi ve root kullanıcı yok; saldırı yüzeyi küçük ve imaj küçük.
- **Healthcheck:** Distroless imajda curl olmadığı için binary'nin kendi `healthcheck` alt komutu kullanılıyor.
- **Zaman dilimi verisi:** `-tags timetzdata` ile binary'ye gömülü.
- **Başlatma sırası:** `depends_on: service_healthy` ile kuruldu; `up --wait` bu zinciri bekliyor.

## 15. Testler: Testcontainers, `TEST_DATABASE_URL` ile de çalışabiliyor

- **Neden:** Her test paketi kendi `postgres:16-alpine` container'ını alıyor; testler yalıtılmış ve paralel çalışıyor. Aynı testler Docker olmayan bir ortamda mevcut bir veritabanına karşı da çalışabiliyor; bu proje Docker'sız bir makinede geliştirildi.
- **Docker yoksa:** Testler atlanıyor. CI'da `TESTCONTAINERS_REQUIRED=1` ayarlanırsa atlamak yerine başarısız oluyor, böylece sessizce geçemiyor.

## 16. Simülatör ve doğrulama aracı: ayrı modül

- **Neden:** Backend'in iç paketlerini import edemiyor; API'ye yalnızca dışarıdan, gerçek bir istemci gibi erişiyor. Bu, kara kutu test için şart. Tek bağımlılığı `coder/websocket`.
- **`make verify` container içinde çalışıyor:** Bu yüzden host'ta Go kurulu olması gerekmiyor.
- **Seed hesapları:** `.invalid` alan adını (RFC 2606) kullanıyor; gerçek bir posta kutusuna asla ulaşamaz. İlk seed kullanıcısı `seed.dalga@ddo-case.invalid`, API açılışta admin olarak oluşturuyor.
