package main

import (
	"reflect"
	"testing"
)

// TestOrphanImages: kun Vibe Deploys egne images, som ingen app bruger, ryddes op. Versioner, man kan gå
// tilbage til, images i brug af en container, andre images og apps, der bygger lige nu, røres ikke.
func TestOrphanImages(t *testing.T) {
	out := "vd-blog:v1\taaa111\t120MB\n" + // ryddet op (Pruned), men stadig der → fjernes
		"vd-blog:v2\tbbb222\t121MB\n" + // kan rulles tilbage til → beholdes
		"vd-blog:v3\tccc333\t122MB\n" + // kører → beholdes
		"vd-slettet:v4\tddd444\t50MB\n" + // appen findes ikke mere → fjernes
		"vd-bygger:v2\teee555\t10MB\n" + // appen bygger lige nu → beholdes
		"vd-andet:v1\tfff666\t5MB\n" + // bruges af en container → beholdes
		"nginx:latest\t999999\t180MB\n" + // ikke vores → beholdes
		"<none>:<none>\t888888\t1.5GB\n" // lag uden navn (tages af docker image prune)
	imgs := parseDockerImages(out)
	if len(imgs) != 8 || imgs[0].Size != 120e6 || imgs[7].Size != 1.5e9 {
		t.Fatalf("parseDockerImages: %+v", imgs)
	}
	db := DB{Apps: map[string]*App{
		"blog": {Status: StatusLive, Versions: []Version{
			{N: 1, Image: "vd-blog:v1", Pruned: true}, {N: 2, Image: "vd-blog:v2"}, {N: 3, Image: "vd-blog:v3"}}},
		"bygger": {Status: StatusBuilding, Versions: []Version{{N: 1, Image: "vd-bygger:v1"}}},
		"andet":  {Status: StatusStopped},
	}}
	keep, busy := keepImages(db)
	inUse := parseContainerImages("vd-andet:v1\nvd-blog:v3\n")
	var got []string
	for _, im := range orphanImages(imgs, keep, busy, inUse) {
		got = append(got, im.Ref)
	}
	if want := []string{"vd-blog:v1", "vd-slettet:v4"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("orphanImages = %v, ville have %v", got, want)
	}
}

// TestParseReclaimed: begge måder, Docker skriver det frigjorte på.
func TestParseReclaimed(t *testing.T) {
	cases := map[string]int64{
		"ID\tRECLAIMABLE\tSIZE\nabc\ttrue\t10MB\nTotal:\t1.2GB\n":              1.2e9,
		"Deleted Images:\ndeleted: sha256:abc\n\nTotal reclaimed space: 512MB": 512e6,
		"Total reclaimed space: 0B":                                            0,
		"":                                                                     0,
	}
	for in, want := range cases {
		if got := parseReclaimed(in); got != want {
			t.Errorf("parseReclaimed(%q) = %d, ville have %d", in, got, want)
		}
	}
}

// TestCacheInfoDemo: uden Docker vises kun en forklaring, og oprydning kan ikke startes.
func TestCacheInfoDemo(t *testing.T) {
	m := newTestManager(t)
	s := &Server{st: m.st, mgr: m, rt: m.rt}
	if v := s.cacheInfo(nil); v.Docker {
		t.Fatal("demo-runtime'n blev regnet som Docker")
	}
	if s.startCleanup("Anna") {
		t.Fatal("oprydning startede uden Docker")
	}
}
