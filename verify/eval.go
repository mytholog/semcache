package verify

// Counts — матрица ошибок двухстадийного кэша.
type Counts struct {
	TP, FP, FN, TN int
	VerifyCalls    int
}

func (c Counts) HitRate() float64 {
	return ratio(c.TP, c.TP+c.FN)
}

func (c Counts) FalseHit() float64 {
	return ratio(c.FP, c.FP+c.TN)
}

func (c Counts) Precision() float64 {
	return ratio(c.TP, c.TP+c.FP)
}

func (c Counts) F1() float64 {
	p, r := c.Precision(), c.HitRate()
	if p+r == 0 {
		return 0
	}
	return 2 * p * r / (p + r)
}

func ratio(num, den int) float64 {
	if den == 0 {
		return 0
	}
	return float64(num) / float64(den)
}

// Evaluate применяет «сначала порог retrieval, потом верификатор».
// ok[i] имеет смысл только если sim[i] >= retrieveMin; иначе это промах.
func Evaluate(sim []float64, interchangeable []bool, retrieveMin float64, ok []bool) Counts {
	var c Counts
	for i := range sim {
		retrieved := sim[i] >= retrieveMin
		if retrieved {
			c.VerifyCalls++
		}
		hit := retrieved && ok[i]
		switch {
		case interchangeable[i] && hit:
			c.TP++
		case interchangeable[i] && !hit:
			c.FN++
		case !interchangeable[i] && hit:
			c.FP++
		default:
			c.TN++
		}
	}
	return c
}

// Cost — грубая экономика одного прогона.
type Cost struct {
	Hits            int
	VerifyCalls     int
	VerifyCacheHits int
	JudgeTokens     int
	ProviderUSD     float64
	VerifyUSD       float64
}

func (c Cost) SavedUSD() float64 {
	return float64(c.Hits)*c.ProviderUSD - c.VerifyUSD
}

func (c Cost) VerifyShare() float64 {
	saved := float64(c.Hits) * c.ProviderUSD
	if saved <= 0 {
		return 0
	}
	return c.VerifyUSD / saved
}
