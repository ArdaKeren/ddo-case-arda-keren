package main

import "fmt"

// FirstSeedUser is the first account in a freshly seeded system. The backend
// creates it as the bootstrap admin (ADMIN_EMAIL / ADMIN_PASSWORD), and the
// simulator uses it to build everything else. The .invalid TLD (RFC 2606)
// guarantees no real mailbox can ever receive mail for these accounts.
const (
	FirstSeedUser = "seed.dalga@ddo-case.invalid"
	DisplayUser   = "seed.display@ddo-case.invalid"
)

func customerEmail(i int) string { return fmt.Sprintf("seed.customer.%04d@ddo-case.invalid", i) }

func branchCode(i int) string { return fmt.Sprintf("SB-%02d", i) }

// 50 provinces, used to name the simulated branches.
var cities = []string{
	"Adana", "Adıyaman", "Afyonkarahisar", "Ağrı", "Aksaray", "Amasya", "Ankara", "Antalya", "Ardahan", "Artvin",
	"Aydın", "Balıkesir", "Bartın", "Batman", "Bayburt", "Bilecik", "Bingöl", "Bitlis", "Bolu", "Burdur",
	"Bursa", "Çanakkale", "Çankırı", "Çorum", "Denizli", "Diyarbakır", "Düzce", "Edirne", "Elazığ", "Erzincan",
	"Erzurum", "Eskişehir", "Gaziantep", "Giresun", "Gümüşhane", "Hakkari", "Hatay", "Iğdır", "Isparta", "İstanbul",
	"İzmir", "Kahramanmaraş", "Karabük", "Karaman", "Kars", "Kastamonu", "Kayseri", "Kırıkkale", "Kırklareli", "Kırşehir",
}

type serviceDef struct{ Code, Name string }

var services = []serviceDef{
	{"A", "Genel İşlemler"},
	{"B", "Hesap İşlemleri"},
	{"C", "Kredi ve Kart"},
}
