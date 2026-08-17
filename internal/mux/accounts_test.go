package mux

import (
	"testing"
	"time"
)

func TestPlanLabel(t *testing.T) {
	tests := map[string]string{
		"free":       "Free",
		"go":         "Go",
		"plus":       "Plus",
		"prolite":    "Pro 5x",
		"pro":        "Pro 20x",
		"business":   "Business",
		"enterprise": "Enterprise",
		"edu":        "Edu",
		"unknown":    "",
	}
	for planType, want := range tests {
		if got := planLabel(planType); got != want {
			t.Errorf("planLabel(%q) = %q, want %q", planType, got, want)
		}
	}
}

func TestLongestAndShortestWindowUsesQuotaDuration(t *testing.T) {
	shortMinutes := int64(300)
	weeklyMinutes := int64(10_080)
	short := &RateLimitWindow{UsedPercent: 72, WindowDurationMins: &shortMinutes}
	weekly := &RateLimitWindow{UsedPercent: 31, WindowDurationMins: &weeklyMinutes}

	longest, shortest := longestAndShortestWindow(&RateLimits{
		Primary: short, Secondary: weekly,
	})
	if longest != weekly || shortest != short {
		t.Fatalf("windows were not ordered by duration: longest=%#v shortest=%#v", longest, shortest)
	}
}

func TestLongestAndShortestWindowHandlesSingleWindow(t *testing.T) {
	minutes := int64(300)
	only := &RateLimitWindow{UsedPercent: 12, WindowDurationMins: &minutes}
	longest, shortest := longestAndShortestWindow(&RateLimits{Primary: only})
	if longest != only || shortest != only {
		t.Fatalf("single window should serve both roles: longest=%#v shortest=%#v", longest, shortest)
	}
}

func TestRateLimitsRequireEveryAdvertisedWindowToHaveCapacity(t *testing.T) {
	shortMinutes := int64(300)
	weeklyMinutes := int64(10_080)
	if rateLimitsHaveCapacity(&RateLimits{
		Primary:   &RateLimitWindow{UsedPercent: 100, WindowDurationMins: &shortMinutes},
		Secondary: &RateLimitWindow{UsedPercent: 20, WindowDurationMins: &weeklyMinutes},
	}) {
		t.Fatal("short-window exhaustion was treated as available capacity")
	}
	if !rateLimitsHaveCapacity(&RateLimits{
		Primary:   &RateLimitWindow{UsedPercent: 50, WindowDurationMins: &shortMinutes},
		Secondary: &RateLimitWindow{UsedPercent: 20, WindowDurationMins: &weeklyMinutes},
	}) {
		t.Fatal("account with capacity in both windows was rejected")
	}
}

func TestDataBoundarySeparatesOrganizationsAndPersonalPlans(t *testing.T) {
	personal := AccountSnapshot{PlanType: "plus", WorkspaceID: "personal-a"}
	personalPro := AccountSnapshot{PlanType: "pro", WorkspaceID: "personal-b"}
	business := AccountSnapshot{PlanType: "business", WorkspaceID: "workspace-a"}
	businessSame := AccountSnapshot{PlanType: "team", WorkspaceID: "workspace-a"}
	businessOther := AccountSnapshot{PlanType: "business", WorkspaceID: "workspace-b"}
	enterprise := AccountSnapshot{PlanType: "enterprise", WorkspaceID: "workspace-a"}
	if !sameDataBoundary(personal, personalPro) {
		t.Fatal("personal subscriptions in the explicit pool should remain compatible")
	}
	if sameDataBoundary(personal, business) {
		t.Fatal("personal and business subscriptions shared a data boundary")
	}
	if !sameDataBoundary(business, businessSame) {
		t.Fatal("the same business workspace was rejected")
	}
	if sameDataBoundary(business, businessOther) || sameDataBoundary(business, enterprise) {
		t.Fatal("cross-workspace or cross-plan-class migration was allowed")
	}
	if sameDataBoundary(
		AccountSnapshot{PlanType: "business"},
		AccountSnapshot{PlanType: "business"},
	) {
		t.Fatal("organization migration was allowed without a verified workspace ID")
	}
}

func TestAggregateRateLimitsKeepsPoolAvailable(t *testing.T) {
	weeklyMinutes := int64(10_080)
	limits, err := aggregateRateLimits([]AccountSnapshot{
		{
			ID: "one", Enabled: true, Connected: true, AuthType: "chatgpt", PlanType: "plus",
			RateLimits: &RateLimits{Primary: &RateLimitWindow{
				UsedPercent: 100, WindowDurationMins: &weeklyMinutes,
			}},
		},
		{
			ID: "two", Enabled: true, Connected: true, AuthType: "chatgpt", PlanType: "plus",
			RateLimits: &RateLimits{Primary: &RateLimitWindow{
				UsedPercent: 20, WindowDurationMins: &weeklyMinutes,
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if limits.Primary == nil || limits.Primary.UsedPercent != 60 {
		t.Fatalf("expected pooled usage to average to 60%%, got %#v", limits.Primary)
	}
	if limits.RateLimitReachedType != nil {
		t.Fatalf("pool should remain available while one account has capacity: %#v", limits)
	}
}

func TestAggregateRateLimitsReportsAllDepleted(t *testing.T) {
	limits, err := aggregateRateLimits([]AccountSnapshot{
		{
			ID: "one", Enabled: true, Connected: true, AuthType: "chatgpt", PlanType: "plus",
			RateLimits: &RateLimits{Primary: &RateLimitWindow{UsedPercent: 100}},
		},
		{
			ID: "two", Enabled: true, Connected: true, AuthType: "chatgpt", PlanType: "plus",
			RateLimits: &RateLimits{Primary: &RateLimitWindow{UsedPercent: 100}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if limits.RateLimitReachedType != "rate_limit_reached" {
		t.Fatalf("expected the pool to report depletion, got %#v", limits)
	}
}

func TestAggregateRateLimitsRejectsMixedPlanPercentages(t *testing.T) {
	_, err := aggregateRateLimits([]AccountSnapshot{
		{ID: "plus", Enabled: true, Connected: true, AuthType: "chatgpt", PlanType: "plus", RateLimits: &RateLimits{}},
		{ID: "pro", Enabled: true, Connected: true, AuthType: "chatgpt", PlanType: "pro", RateLimits: &RateLimits{}},
	})
	if err == nil {
		t.Fatal("mixed Plus and Pro percentages were presented as one capacity")
	}
}

func TestRouteUrgencyPrefersQuotaExpiringSooner(t *testing.T) {
	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
	weeklyMinutes := int64(10_080)
	soon := now.Add(24 * time.Hour).Unix()
	later := now.Add(6 * 24 * time.Hour).Unix()
	soonScore := routeUrgencyScore(now, &RateLimitWindow{
		UsedPercent: 40, WindowDurationMins: &weeklyMinutes, ResetsAt: &soon,
	}, resetCreditMetadata{})
	laterScore := routeUrgencyScore(now, &RateLimitWindow{
		UsedPercent: 40, WindowDurationMins: &weeklyMinutes, ResetsAt: &later,
	}, resetCreditMetadata{})
	if soonScore <= laterScore {
		t.Fatalf("sooner reset should be more urgent: soon=%f later=%f", soonScore, laterScore)
	}
}

func TestRouteUrgencyWeightsBankedResetsWithoutDominating(t *testing.T) {
	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
	weeklyMinutes := int64(10_080)
	reset := now.Add(4 * 24 * time.Hour).Unix()
	window := &RateLimitWindow{
		UsedPercent: 50, WindowDurationMins: &weeklyMinutes, ResetsAt: &reset,
	}
	plain := routeUrgencyScore(now, window, resetCreditMetadata{Known: true})
	banked := routeUrgencyScore(now, window, resetCreditMetadata{Known: true, AvailableCount: 2})
	if banked <= plain {
		t.Fatalf("banked resets should increase urgency: plain=%f banked=%f", plain, banked)
	}
	if banked > plain*1.31 {
		t.Fatalf("banked reset bonus should remain bounded: plain=%f banked=%f", plain, banked)
	}
}

func TestRouteUrgencyCapsResetBonus(t *testing.T) {
	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
	reset := now.Add(7 * 24 * time.Hour).Unix()
	window := &RateLimitWindow{UsedPercent: 20, ResetsAt: &reset}
	three := routeUrgencyScore(now, window, resetCreditMetadata{Known: true, AvailableCount: 3})
	ten := routeUrgencyScore(now, window, resetCreditMetadata{Known: true, AvailableCount: 10})
	if three != ten {
		t.Fatalf("reset bonus cap was not applied: three=%f ten=%f", three, ten)
	}
}

func TestRouteUrgencyFallsBackToWeeklyUtilization(t *testing.T) {
	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
	weeklyMinutes := int64(10_080)
	lessUsed := routeUrgencyScore(now, &RateLimitWindow{
		UsedPercent: 20, WindowDurationMins: &weeklyMinutes,
	}, resetCreditMetadata{})
	moreUsed := routeUrgencyScore(now, &RateLimitWindow{
		UsedPercent: 80, WindowDurationMins: &weeklyMinutes,
	}, resetCreditMetadata{})
	if lessUsed <= moreUsed {
		t.Fatalf("fallback should prefer the less-used account: less=%f more=%f", lessUsed, moreUsed)
	}
}
