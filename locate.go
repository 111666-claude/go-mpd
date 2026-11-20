package mpd

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Errors returned by MPD.Locate. They are wrapped with context, so use
// errors.Is to test for them.
var (
	// ErrInvalidTime is returned when the requested playback time is
	// negative, NaN or infinite.
	ErrInvalidTime = errors.New("mpd: playback time must be finite and >= 0")
	// ErrRepresentationNotFound is returned when the given Representation
	// is not part of the MPD.
	ErrRepresentationNotFound = errors.New("mpd: representation not found in MPD")
	// ErrNoSegmentTemplate is returned when neither the Representation nor
	// its AdaptationSet carries a SegmentTemplate.
	ErrNoSegmentTemplate = errors.New("mpd: no SegmentTemplate for representation")
	// ErrNoSegmentTiming is returned when the SegmentTemplate has neither
	// a duration nor a SegmentTimeline.
	ErrNoSegmentTiming = errors.New("mpd: SegmentTemplate has neither duration nor SegmentTimeline")
	// ErrZeroTimescale is returned when a SegmentTemplate needed for the
	// lookup declares timescale="0".
	ErrZeroTimescale = errors.New("mpd: SegmentTemplate timescale is zero")
	// ErrZeroDuration is returned when a SegmentTemplate declares
	// duration="0".
	ErrZeroDuration = errors.New("mpd: SegmentTemplate duration is zero")
	// ErrInfiniteRepeat is returned when a SegmentTimeline contains an
	// S element with r="-1", which cannot be enumerated.
	ErrInfiniteRepeat = errors.New("mpd: SegmentTimeline contains infinite repeat (r=-1)")
	// ErrMissingTemplateVariable is returned when a required template
	// variable ($Number$, $Time$, $RepresentationID$) is absent from the
	// template or cannot be supplied.
	ErrMissingTemplateVariable = errors.New("mpd: missing template variable")
	// ErrUnknownTemplateVariable is returned for template identifiers the
	// locator cannot substitute (e.g. $Bandwidth$).
	ErrUnknownTemplateVariable = errors.New("mpd: unknown template variable")
	// ErrOutOfRange is returned when the requested time falls outside the
	// presentation: before/after the owning Period, past the end of the
	// SegmentTimeline, or beyond the live window.
	ErrOutOfRange = errors.New("mpd: playback time out of range")
)

// SegmentLocation describes the media segment that contains a requested
// playback time, plus the initialization segment of its Representation.
// All times are presentation times in seconds relative to the start of
// the MPD.
type SegmentLocation struct {
	// PeriodIndex is the index of the Period owning the Representation.
	PeriodIndex int
	// InitializationURL is the resolved URL of the initialization segment.
	InitializationURL string
	// MediaURL is the resolved URL of the media segment containing the
	// requested time.
	MediaURL string
	// Number is the value substituted for $Number$.
	Number uint64
	// Time is the value substituted for $Time$ (SegmentTimeline only,
	// zero otherwise).
	Time uint64
	// Start and End are the presentation time interval covered by the
	// media segment, in seconds.
	Start float64
	End   float64
}

var (
	templateVarRE = regexp.MustCompile(`\$\$|\$([A-Za-z]+)(?:%0(\d+)d)?\$`)
	numberVarRE   = regexp.MustCompile(`\$Number(%0\d+d)?\$`)
	timeVarRE     = regexp.MustCompile(`\$Time(%0\d+d)?\$`)
)

// Locate resolves the initialization and media segment URLs of rep for
// the given playback time in seconds. rep must be a Representation of m
// (matched by pointer); the owning Period and AdaptationSet are derived
// from it, so multi-Period MPDs never mix template state across Period
// boundaries. The MPD is only read, never modified.
func (m *MPD) Locate(rep *Representation, seconds float64) (*SegmentLocation, error) {
	if rep == nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 {
		return nil, ErrInvalidTime
	}

	periodIdx, as, err := m.findRepresentation(rep)
	if err != nil {
		return nil, err
	}
	period := m.Period[periodIdx]

	tpl := rep.SegmentTemplate
	if tpl == nil {
		tpl = as.SegmentTemplate
	}
	if tpl == nil {
		return nil, ErrNoSegmentTemplate
	}

	timescale := uint64(1)
	if tpl.Timescale != nil {
		if *tpl.Timescale == 0 {
			return nil, ErrZeroTimescale
		}
		timescale = *tpl.Timescale
	}
	ts := float64(timescale)

	periodStart, periodEnd, err := m.periodBounds(periodIdx)
	if err != nil {
		return nil, err
	}
	if seconds < periodStart || seconds >= periodEnd {
		return nil, fmt.Errorf("%w: %.3fs not in period %d [%.3f, %.3f)",
			ErrOutOfRange, seconds, periodIdx, periodStart, periodEnd)
	}
	rel := seconds - periodStart

	base, err := resolveBaseURLs(m.BaseURL, period.BaseURL, as.BaseURL, rep.BaseURL)
	if err != nil {
		return nil, err
	}

	repID := ""
	if rep.ID != nil {
		repID = *rep.ID
	}

	loc := &SegmentLocation{PeriodIndex: periodIdx}

	if tpl.Initialization != nil {
		init, err := substituteTemplate(*tpl.Initialization, repID, rep.ID != nil, 0, 0, false)
		if err != nil {
			return nil, err
		}
		loc.InitializationURL, err = joinURL(base, init)
		if err != nil {
			return nil, err
		}
	}

	if tpl.Media == nil {
		return nil, fmt.Errorf("%w: SegmentTemplate has no media attribute", ErrMissingTemplateVariable)
	}
	media := *tpl.Media

	startNumber := uint64(1)
	if tpl.StartNumber != nil {
		startNumber = *tpl.StartNumber
	}
	var pto uint64
	if tpl.PresentationTimeOffset != nil {
		pto = *tpl.PresentationTimeOffset
	}

	if tpl.SegmentTimeline != nil {
		if !timeVarRE.MatchString(media) && !numberVarRE.MatchString(media) {
			return nil, fmt.Errorf("%w: media template needs $Time$ or $Number$ with SegmentTimeline", ErrMissingTemplateVariable)
		}
		err = locateInTimeline(tpl, startNumber, pto, ts, rel, periodStart, loc)
	} else {
		if tpl.Duration == nil {
			return nil, ErrNoSegmentTiming
		}
		if *tpl.Duration == 0 {
			return nil, ErrZeroDuration
		}
		if !numberVarRE.MatchString(media) {
			return nil, fmt.Errorf("%w: media template needs $Number$ with duration-based addressing", ErrMissingTemplateVariable)
		}
		segDur := float64(*tpl.Duration) / ts
		idx := uint64(math.Floor(rel / segDur))
		loc.Number = startNumber + idx
		loc.Start = periodStart + float64(idx)*segDur
		loc.End = loc.Start + segDur
	}
	if err != nil {
		return nil, err
	}

	mediaURL, err := substituteTemplate(media, repID, rep.ID != nil, loc.Number, loc.Time, true)
	if err != nil {
		return nil, err
	}
	loc.MediaURL, err = joinURL(base, mediaURL)
	if err != nil {
		return nil, err
	}
	return loc, nil
}

// findRepresentation locates rep by pointer identity and returns its
// Period index and AdaptationSet.
func (m *MPD) findRepresentation(rep *Representation) (int, *AdaptationSet, error) {
	for pi, p := range m.Period {
		for _, as := range p.AdaptationSets {
			for i := range as.Representations {
				if &as.Representations[i] == rep {
					return pi, as, nil
				}
			}
		}
	}
	return 0, nil, ErrRepresentationNotFound
}

// periodBounds returns the [start, end) presentation interval of the
// period in seconds. The end is derived from the period duration, the
// next period's start, or the media presentation duration; if none of
// these exist it is +Inf (typical for the last period of a live MPD).
func (m *MPD) periodBounds(idx int) (float64, float64, error) {
	start := 0.0
	for i := 0; i <= idx; i++ {
		p := m.Period[i]
		if p.Start != nil {
			s, err := p.Start.ToSeconds()
			if err != nil {
				return 0, 0, err
			}
			start = s
		} else if i > 0 {
			// start stays at the previous period's end
		}
		if i == idx {
			break
		}
		end, err := m.periodEnd(i, start)
		if err != nil {
			return 0, 0, err
		}
		start = end
	}
	end, err := m.periodEnd(idx, start)
	if err != nil {
		return 0, 0, err
	}
	return start, end, nil
}

func (m *MPD) periodEnd(idx int, start float64) (float64, error) {
	p := m.Period[idx]
	if p.Duration != nil {
		d, err := p.Duration.ToSeconds()
		if err != nil {
			return 0, err
		}
		return start + d, nil
	}
	if idx+1 < len(m.Period) && m.Period[idx+1].Start != nil {
		s, err := m.Period[idx+1].Start.ToSeconds()
		if err != nil {
			return 0, err
		}
		return s, nil
	}
	if m.MediaPresentationDuration != nil {
		d, err := m.MediaPresentationDuration.ToSeconds()
		if err != nil {
			return 0, err
		}
		return d, nil
	}
	return math.Inf(1), nil
}

// locateInTimeline walks the SegmentTimeline and fills loc with the
// segment covering rel seconds after the period start. Repeats (r >= 0)
// are expanded logically, not into a slice.
func locateInTimeline(tpl *SegmentTemplate, startNumber, pto uint64, ts, rel, periodStart float64, loc *SegmentLocation) error {
	target := rel*ts + float64(pto)

	var t uint64 // media time of the current S run
	number := startNumber
	first := true

	for _, s := range tpl.SegmentTimeline.S {
		if s.R != nil && *s.R < 0 {
			return ErrInfiniteRepeat
		}
		if s.T != nil {
			t = *s.T
		} else if first {
			t = pto
		}
		first = false

		repeats := uint64(1)
		if s.R != nil {
			repeats += uint64(*s.R)
		}
		for k := uint64(0); k < repeats; k++ {
			segStart := t + k*s.D
			segEnd := segStart + s.D
			if float64(target) >= float64(segStart) && float64(target) < float64(segEnd) {
				loc.Number = number
				loc.Time = segStart
				loc.Start = periodStart + float64(segStart-pto)/ts
				loc.End = periodStart + float64(segEnd-pto)/ts
				return nil
			}
			number++
		}
		t += repeats * s.D
	}
	return fmt.Errorf("%w: %.3fs is past the end of the SegmentTimeline", ErrOutOfRange, rel+periodStart)
}

// substituteTemplate expands $Identifier$ and $Identifier%0Nd$ placeholders.
// Allowed identifiers: RepresentationID, Number (only if allowNumberTime),
// Time (only if allowNumberTime). "$$" escapes a literal dollar sign.
func substituteTemplate(tpl, repID string, hasRepID bool, number, timeVal uint64, allowNumberTime bool) (string, error) {
	var subErr error
	out := templateVarRE.ReplaceAllStringFunc(tpl, func(match string) string {
		if subErr != nil {
			return ""
		}
		if match == "$$" {
			return "$"
		}
		parts := templateVarRE.FindStringSubmatch(match)
		name, width := parts[1], parts[2]
		var v uint64
		switch name {
		case "RepresentationID":
			if !hasRepID {
				subErr = fmt.Errorf("%w: $RepresentationID$ used but representation has no id", ErrMissingTemplateVariable)
				return ""
			}
			return repID
		case "Number":
			if !allowNumberTime {
				subErr = fmt.Errorf("%w: $Number$ not allowed here", ErrMissingTemplateVariable)
				return ""
			}
			v = number
		case "Time":
			if !allowNumberTime {
				subErr = fmt.Errorf("%w: $Time$ not allowed here", ErrMissingTemplateVariable)
				return ""
			}
			v = timeVal
		default:
			subErr = fmt.Errorf("%w: $%s$", ErrUnknownTemplateVariable, name)
			return ""
		}
		if width != "" {
			w, _ := strconv.Atoi(width)
			return fmt.Sprintf("%0*d", w, v)
		}
		return strconv.FormatUint(v, 10)
	})
	if subErr != nil {
		return "", subErr
	}
	return out, nil
}

// resolveBaseURLs folds the BaseURL chain (MPD → Period → AdaptationSet →
// Representation) into a single base, resolving relative references per
// RFC 3986. The first URL of each level is used.
func resolveBaseURLs(levels ...[]*BaseURL) (string, error) {
	var base *url.URL
	for _, level := range levels {
		if len(level) == 0 {
			continue
		}
		ref, err := url.Parse(strings.TrimSpace(level[0].Value))
		if err != nil {
			return "", err
		}
		if base == nil {
			base = ref
		} else {
			base = base.ResolveReference(ref)
		}
	}
	if base == nil {
		return "", nil
	}
	return base.String(), nil
}

// joinURL resolves a segment URL against the accumulated base.
func joinURL(base, ref string) (string, error) {
	if base == "" {
		return ref, nil
	}
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	r, err := url.Parse(ref)
	if err != nil {
		return "", err
	}
	return b.ResolveReference(r).String(), nil
}
