package mpd

import (
	"errors"
	"io/ioutil"
	"math"

	"github.com/unki2aut/go-xsd-types"
	. "gopkg.in/check.v1"
)

// ErrorIs is a gocheck checker based on errors.Is, so wrapped sentinel
// errors (fmt.Errorf("%w: ...")) match their sentinel.
var ErrorIs Checker = &errorIsChecker{
	&CheckerInfo{Name: "ErrorIs", Params: []string{"obtained", "expected"}},
}

type errorIsChecker struct{ *CheckerInfo }

func (checker *errorIsChecker) Check(params []interface{}, names []string) (bool, string) {
	obtained, ok := params[0].(error)
	if !ok {
		return false, "obtained is not an error"
	}
	expected, ok := params[1].(error)
	if !ok {
		return false, "expected is not an error"
	}
	return errors.Is(obtained, expected), ""
}

type LocateSuite struct{}

var _ = Suite(&LocateSuite{})

func readFixture(c *C, name string) *MPD {
	b, err := ioutil.ReadFile(name)
	c.Assert(err, IsNil)
	mpd := new(MPD)
	c.Assert(mpd.Decode(b), IsNil)
	return mpd
}

func repAt(c *C, m *MPD, period, as, rep int) *Representation {
	return &m.Period[period].AdaptationSets[as].Representations[rep]
}

// Period 0: Number template, timescale=1000, duration=4000 (4s segments),
// startNumber=7. 30s -> segment index 7 -> number 14, covering [28, 32).
func (s *LocateSuite) TestLocateNumberTemplate(c *C) {
	m := readFixture(c, "fixtures/locate_multi_period.mpd")
	loc, err := m.Locate(repAt(c, m, 0, 0, 0), 30)
	c.Assert(err, IsNil)
	c.Check(loc.PeriodIndex, Equals, 0)
	c.Check(loc.InitializationURL, Equals, "https://cdn.example.com/vod/p0/v1/init.mp4")
	c.Check(loc.MediaURL, Equals, "https://cdn.example.com/vod/p0/v1/00014.m4s")
	c.Check(loc.Number, Equals, uint64(14))
	c.Check(loc.Start, Equals, 28.0)
	c.Check(loc.End, Equals, 32.0)
}

// Period 1: Time template, timescale=90000, timeline
//
//	S t=0 d=180000 r=1  -> [60,62) #100 t=0,  [62,64) #101 t=180000
//	S d=270000          -> [64,67) #102 t=360000
//
// 63s hits the second repeat of the first S; the relative BaseURL
// "period1/" must resolve against the MPD-level BaseURL.
func (s *LocateSuite) TestLocateTimeTemplateRepeatedTimeline(c *C) {
	m := readFixture(c, "fixtures/locate_multi_period.mpd")
	loc, err := m.Locate(repAt(c, m, 1, 0, 0), 63)
	c.Assert(err, IsNil)
	c.Check(loc.PeriodIndex, Equals, 1)
	c.Check(loc.InitializationURL, Equals, "https://cdn.example.com/vod/period1/init_v2.mp4")
	c.Check(loc.MediaURL, Equals, "https://cdn.example.com/vod/period1/chunk_180000.m4s")
	c.Check(loc.Number, Equals, uint64(101))
	c.Check(loc.Time, Equals, uint64(180000))
	c.Check(loc.Start, Equals, 62.0)
	c.Check(loc.End, Equals, 64.0)
}

// Segment right after the repeated run: third timeline segment.
func (s *LocateSuite) TestLocateTimelineAfterRepeat(c *C) {
	m := readFixture(c, "fixtures/locate_multi_period.mpd")
	loc, err := m.Locate(repAt(c, m, 1, 0, 0), 65)
	c.Assert(err, IsNil)
	c.Check(loc.MediaURL, Equals, "https://cdn.example.com/vod/period1/chunk_360000.m4s")
	c.Check(loc.Number, Equals, uint64(102))
	c.Check(loc.Start, Equals, 64.0)
	c.Check(loc.End, Equals, 67.0)
}

// Boundary: exactly 60s is the first segment of period 1, not the last
// of period 0.
func (s *LocateSuite) TestLocatePeriodBoundary(c *C) {
	m := readFixture(c, "fixtures/locate_multi_period.mpd")
	loc, err := m.Locate(repAt(c, m, 1, 0, 0), 60)
	c.Assert(err, IsNil)
	c.Check(loc.Number, Equals, uint64(100))
	c.Check(loc.Time, Equals, uint64(0))
	c.Check(loc.Start, Equals, 60.0)
	c.Check(loc.End, Equals, 62.0)

	// 59.999s still belongs to period 0: index 14 -> number 21, [56, 60).
	loc, err = m.Locate(repAt(c, m, 0, 0, 0), 59.999)
	c.Assert(err, IsNil)
	c.Check(loc.Number, Equals, uint64(21))
	c.Check(loc.Start, Equals, 56.0)
	c.Check(loc.End, Equals, 60.0)
}

// Locate must not mutate the MPD.
func (s *LocateSuite) TestLocateDoesNotModifyMPD(c *C) {
	m := readFixture(c, "fixtures/locate_multi_period.mpd")
	before, err := m.Encode()
	c.Assert(err, IsNil)
	for _, sec := range []float64{0, 30, 60, 63, 65, 119} {
		m.Locate(repAt(c, m, 0, 0, 0), sec)
		m.Locate(repAt(c, m, 1, 0, 0), sec)
	}
	after, err := m.Encode()
	c.Assert(err, IsNil)
	c.Check(string(after), Equals, string(before))
}

func (s *LocateSuite) TestLocateInvalidTime(c *C) {
	m := readFixture(c, "fixtures/locate_multi_period.mpd")
	rep := repAt(c, m, 0, 0, 0)
	for _, sec := range []float64{-1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		_, err := m.Locate(rep, sec)
		c.Check(err, ErrorIs, ErrInvalidTime, Commentf("seconds=%v", sec))
	}
	_, err := m.Locate(nil, 10)
	c.Check(err, ErrorIs, ErrInvalidTime)
}

func (s *LocateSuite) TestLocateOutOfRange(c *C) {
	m := readFixture(c, "fixtures/locate_multi_period.mpd")
	// Past the presentation duration.
	_, err := m.Locate(repAt(c, m, 1, 0, 0), 200)
	c.Check(err, ErrorIs, ErrOutOfRange)
	// Inside period 1 but past the end of its SegmentTimeline (67s).
	_, err = m.Locate(repAt(c, m, 1, 0, 0), 70)
	c.Check(err, ErrorIs, ErrOutOfRange)
	// Time falls into the other period: no cross-period guessing.
	_, err = m.Locate(repAt(c, m, 0, 0, 0), 90)
	c.Check(err, ErrorIs, ErrOutOfRange)
}

func (s *LocateSuite) TestLocateRepresentationNotFound(c *C) {
	m := readFixture(c, "fixtures/locate_multi_period.mpd")
	_, err := m.Locate(&Representation{}, 10)
	c.Check(err, ErrorIs, ErrRepresentationNotFound)
}

func strptr(s string) *string { return &s }
func u64ptr(v uint64) *uint64 { return &v }
func i64ptr(v int64) *int64   { return &v }

func mpdWithTemplate(c *C, tpl *SegmentTemplate) (*MPD, *Representation) {
	start, err := xsd.DurationFromString("PT0S")
	c.Assert(err, IsNil)
	m := &MPD{
		Period: []*Period{{
			Start: start,
			AdaptationSets: []*AdaptationSet{{
				SegmentTemplate: tpl,
				Representations: []Representation{{ID: strptr("r1")}},
			}},
		}},
	}
	return m, &m.Period[0].AdaptationSets[0].Representations[0]
}

func (s *LocateSuite) TestLocateZeroTimescale(c *C) {
	m, rep := mpdWithTemplate(c, &SegmentTemplate{
		Timescale: u64ptr(0),
		Duration:  u64ptr(4000),
		Media:     strptr("$Number$.m4s"),
	})
	_, err := m.Locate(rep, 10)
	c.Check(err, ErrorIs, ErrZeroTimescale)
}

func (s *LocateSuite) TestLocateInfiniteRepeat(c *C) {
	m, rep := mpdWithTemplate(c, &SegmentTemplate{
		Timescale: u64ptr(1000),
		Media:     strptr("$Time$.m4s"),
		SegmentTimeline: &SegmentTimeline{S: []*SegmentTimelineS{
			{T: u64ptr(0), D: 4000, R: i64ptr(-1)},
		}},
	})
	_, err := m.Locate(rep, 10)
	c.Check(err, ErrorIs, ErrInfiniteRepeat)
}

func (s *LocateSuite) TestLocateMissingTemplateVariable(c *C) {
	// duration-based addressing without $Number$
	m, rep := mpdWithTemplate(c, &SegmentTemplate{
		Timescale: u64ptr(1000),
		Duration:  u64ptr(4000),
		Media:     strptr("seg.m4s"),
	})
	_, err := m.Locate(rep, 10)
	c.Check(err, ErrorIs, ErrMissingTemplateVariable)

	// timeline without $Time$ or $Number$
	m, rep = mpdWithTemplate(c, &SegmentTemplate{
		Timescale: u64ptr(1000),
		Media:     strptr("seg.m4s"),
		SegmentTimeline: &SegmentTimeline{S: []*SegmentTimelineS{
			{T: u64ptr(0), D: 4000},
		}},
	})
	_, err = m.Locate(rep, 10)
	c.Check(err, ErrorIs, ErrMissingTemplateVariable)

	// $RepresentationID$ referenced but representation has no id
	m, rep = mpdWithTemplate(c, &SegmentTemplate{
		Timescale: u64ptr(1000),
		Duration:  u64ptr(4000),
		Media:     strptr("$RepresentationID$/$Number$.m4s"),
	})
	rep.ID = nil
	_, err = m.Locate(rep, 10)
	c.Check(err, ErrorIs, ErrMissingTemplateVariable)
}

func (s *LocateSuite) TestLocateUnknownTemplateVariable(c *C) {
	m, rep := mpdWithTemplate(c, &SegmentTemplate{
		Timescale: u64ptr(1000),
		Duration:  u64ptr(4000),
		Media:     strptr("$Bandwidth$_$Number$.m4s"),
	})
	_, err := m.Locate(rep, 10)
	c.Check(err, ErrorIs, ErrUnknownTemplateVariable)
}
