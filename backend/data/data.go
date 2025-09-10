package data

// TimePeriodMultiplier maps a period id (1..n) to a demand multiplier.
// Example semantics (assumed):
// 1 = very early off-peak, 2 = morning peak, 3 = late morning, 4 = mid-day, 5 = evening peak, 6 = late evening.
var TimePeriodMultiplier = map[int]float64{
	1: 0.3,
	2: 1.6,
	3: 0.9,
	4: 0.8,
	5: 1.4,
	6: 0.5,
}