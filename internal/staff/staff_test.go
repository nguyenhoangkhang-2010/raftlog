package staff

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/pgmigrate"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/pgtest"
)

var ctx = context.Background()

// the venue is seven hours ahead of UTC all the year
var venue = time.FixedZone("venue", 7*3600)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }
func (c *clock) set(day string, hh, mm int) {
	d, _ := time.ParseInLocation("2006-01-02", day, venue)
	c.t = time.Date(d.Year(), d.Month(), d.Day(), hh, mm, 0, 0, venue).UTC()
}

func newStore(t *testing.T) (*Store, *clock) {
	t.Helper()
	pool := pgtest.Pool(t)
	if _, err := pgmigrate.Apply(ctx, pool, Migrations(), pgmigrate.WithTable(MigrationsTable)); err != nil {
		t.Fatal(err)
	}
	c := &clock{}
	c.set("2026-11-10", 7, 0)
	return New(pool, venue, c.now), c
}

func at(day string, hh, mm int) time.Time {
	d, _ := time.ParseInLocation("2006-01-02", day, venue)
	return time.Date(d.Year(), d.Month(), d.Day(), hh, mm, 0, 0, venue).UTC()
}

func mustShift(t *testing.T, s *Store, user, day, start, end string) Shift {
	t.Helper()
	sh, err := s.AddShift(ctx, ShiftInput{User: user, Day: day, Start: start, End: end, Station: "line"}, "mgr-1")
	if err != nil {
		t.Fatalf("add shift %s %s %s-%s: %v", user, day, start, end, err)
	}
	return sh
}

func TestAShiftIsPlannedReadAndKeptInTheVenuesTime(t *testing.T) {
	s, _ := newStore(t)
	sh, err := s.AddShift(ctx, ShiftInput{User: "an", Day: "2026-11-10", Start: "08:00", End: "16:30", Station: " line ", Note: " opens "}, "mgr-1")
	if err != nil || sh.Start != "08:00" || sh.End != "16:30" || sh.Station != "line" || sh.Note != "opens" || sh.Status != Planned || sh.CreatedBy != "mgr-1" {
		t.Fatalf("shift: %+v %v", sh, err)
	}
	if !sh.StartsAt.Equal(at("2026-11-10", 8, 0)) || sh.EndsAt.Sub(sh.StartsAt) != 8*time.Hour+30*time.Minute {
		t.Fatalf("moments: %v %v", sh.StartsAt, sh.EndsAt)
	}
	list, err := s.Shifts(ctx, "2026-11-09", "2026-11-11", "", false)
	if err != nil || len(list) != 1 || list[0].ID != sh.ID {
		t.Fatalf("list: %+v %v", list, err)
	}
	if list, _ := s.Shifts(ctx, "2026-11-11", "2026-11-12", "", false); len(list) != 0 {
		t.Fatalf("another day: %+v", list)
	}
	if list, _ := s.Shifts(ctx, "2026-11-09", "2026-11-11", "binh", false); len(list) != 0 {
		t.Fatalf("another person: %+v", list)
	}
}

func TestAShiftThatEndsBeforeItStartsRunsIntoTheNextDay(t *testing.T) {
	s, _ := newStore(t)
	sh := mustShift(t, s, "an", "2026-11-10", "18:00", "02:00")
	if sh.Day != "2026-11-10" || sh.EndsAt.Sub(sh.StartsAt) != 8*time.Hour || !sh.EndsAt.Equal(at("2026-11-11", 2, 0)) {
		t.Fatalf("night shift: %+v", sh)
	}
}

func TestShiftsThatAreNotWellFormedAreRefused(t *testing.T) {
	s, _ := newStore(t)
	for name, in := range map[string]ShiftInput{
		"no person":     {Day: "2026-11-10", Start: "08:00", End: "12:00"},
		"bad person":    {User: "An Nguyen", Day: "2026-11-10", Start: "08:00", End: "12:00"},
		"bad day":       {User: "an", Day: "10/11/2026", Start: "08:00", End: "12:00"},
		"not a day":     {User: "an", Day: "2026-02-30", Start: "08:00", End: "12:00"},
		"bad time":      {User: "an", Day: "2026-11-10", Start: "8:00", End: "12:00"},
		"past 24":       {User: "an", Day: "2026-11-10", Start: "08:00", End: "24:00"},
		"too short":     {User: "an", Day: "2026-11-10", Start: "08:00", End: "08:20"},
		"too long":      {User: "an", Day: "2026-11-10", Start: "06:00", End: "23:00"},
		"same time":     {User: "an", Day: "2026-11-10", Start: "08:00", End: "08:00"},
		"long station":  {User: "an", Day: "2026-11-10", Start: "08:00", End: "12:00", Station: "a very long name of a station"},
		"long note":     {User: "an", Day: "2026-11-10", Start: "08:00", End: "12:00", Note: string(make([]rune, 201))},
		"note not text": {User: "an", Day: "2026-11-10", Start: "08:00", End: "12:00", Note: "\xff"},
	} {
		_, err := s.AddShift(ctx, in, "mgr-1")
		var bad *InvalidError
		if !errors.As(err, &bad) {
			t.Errorf("%s: want InvalidError, got %v", name, err)
		}
	}
	if _, err := s.AddShift(ctx, ShiftInput{User: "an", Day: "2026-11-10", Start: "08:00", End: "12:00"}, ""); err == nil {
		t.Error("a shift needs to say who wrote it")
	}
}

func TestTwoShiftsOfOnePersonDoNotOverlapButTheyMayTouch(t *testing.T) {
	s, _ := newStore(t)
	first := mustShift(t, s, "an", "2026-11-10", "08:00", "12:00")
	if _, err := s.AddShift(ctx, ShiftInput{User: "an", Day: "2026-11-10", Start: "11:00", End: "15:00"}, "mgr-1"); !errors.Is(err, ErrOverlap) {
		t.Fatalf("overlap: %v", err)
	}
	mustShift(t, s, "an", "2026-11-10", "12:00", "16:00") // touching is fine
	mustShift(t, s, "binh", "2026-11-10", "08:00", "12:00")
	// a night shift of the day before overlaps the morning
	mustShift(t, s, "chi", "2026-11-09", "22:00", "06:00")
	if _, err := s.AddShift(ctx, ShiftInput{User: "chi", Day: "2026-11-10", Start: "05:00", End: "09:00"}, "mgr-1"); !errors.Is(err, ErrOverlap) {
		t.Fatalf("the night shift overlaps the morning: %v", err)
	}
	// moving a shift onto another is refused, onto its own place is not
	if _, err := s.UpdateShift(ctx, first.ID, ShiftInput{Day: "2026-11-10", Start: "10:00", End: "13:00"}, "mgr-1"); !errors.Is(err, ErrOverlap) {
		t.Fatalf("update onto another: %v", err)
	}
	up, err := s.UpdateShift(ctx, first.ID, ShiftInput{Day: "2026-11-10", Start: "07:00", End: "12:00", Station: "bar", Note: "early"}, "mgr-2")
	if err != nil || up.Start != "07:00" || up.Station != "bar" || up.UpdatedBy != "mgr-2" || up.CreatedBy != "mgr-1" || up.User != "an" {
		t.Fatalf("update: %+v %v", up, err)
	}
}

func TestACancelledShiftStaysOnRecordFreesTheTimeAndCannotBeChanged(t *testing.T) {
	s, _ := newStore(t)
	sh := mustShift(t, s, "an", "2026-11-10", "08:00", "12:00")
	c, err := s.CancelShift(ctx, sh.ID, "mgr-1", " sick ")
	if err != nil || c.Status != Cancelled || c.CancelReason != "sick" || c.CancelledBy != "mgr-1" || c.CancelledAt == nil {
		t.Fatalf("cancel: %+v %v", c, err)
	}
	if list, _ := s.Shifts(ctx, "2026-11-10", "2026-11-10", "", false); len(list) != 0 {
		t.Fatalf("a cancelled shift is not on the schedule: %+v", list)
	}
	if list, _ := s.Shifts(ctx, "2026-11-10", "2026-11-10", "", true); len(list) != 1 || list[0].Status != Cancelled {
		t.Fatalf("but it is on record: %+v", list)
	}
	if _, err := s.CancelShift(ctx, sh.ID, "mgr-1", ""); !errors.Is(err, ErrCancelled) {
		t.Fatalf("cancel again: %v", err)
	}
	if _, err := s.UpdateShift(ctx, sh.ID, ShiftInput{Day: "2026-11-10", Start: "09:00", End: "12:00"}, "mgr-1"); !errors.Is(err, ErrCancelled) {
		t.Fatalf("update a cancelled one: %v", err)
	}
	if _, err := s.CancelShift(ctx, 999999, "mgr-1", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancel nothing: %v", err)
	}
	// the time is free again
	mustShift(t, s, "an", "2026-11-10", "09:00", "12:00")
	// and the database itself says no to changing it or deleting anything
	if _, err := s.pool.Exec(ctx, `UPDATE rota SET note = 'x' WHERE id = $1`, sh.ID); err == nil {
		t.Error("a cancelled shift was changed")
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM rota`); err == nil {
		t.Error("shifts were deleted")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE rota SET user_id = 'binh' WHERE status = 'planned'`); err == nil {
		t.Error("a shift was given to another person")
	}
}

func TestAWeekIsCopiedToTheNextAndWhatWouldOverlapIsLeftOut(t *testing.T) {
	s, _ := newStore(t)
	mustShift(t, s, "an", "2026-11-09", "08:00", "12:00")
	mustShift(t, s, "an", "2026-11-11", "08:00", "12:00")
	mustShift(t, s, "binh", "2026-11-15", "14:00", "22:00")
	mustShift(t, s, "binh", "2026-11-16", "14:00", "22:00") // outside the week
	gone := mustShift(t, s, "chi", "2026-11-10", "08:00", "12:00")
	if _, err := s.CancelShift(ctx, gone.ID, "mgr-1", ""); err != nil {
		t.Fatal(err)
	}
	mustShift(t, s, "an", "2026-11-18", "10:00", "14:00") // is in the way of the copy of the 11th
	made, skipped, err := s.CopyDays(ctx, "2026-11-09", "2026-11-15", 7, "mgr-1")
	if err != nil || made != 2 || skipped != 1 {
		t.Fatalf("copy: made %d skipped %d %v", made, skipped, err)
	}
	next, _ := s.Shifts(ctx, "2026-11-16", "2026-11-22", "", false)
	if len(next) != 4 { // the one of the 16th, the copy of the 9th, of the 15th, and the one of the 18th
		t.Fatalf("next week: %+v", next)
	}
	if _, _, err := s.CopyDays(ctx, "2026-11-09", "2026-11-15", 0, "mgr-1"); err == nil {
		t.Error("copy by 0 days")
	}
	if _, _, err := s.CopyDays(ctx, "2026-11-15", "2026-11-09", 7, "mgr-1"); err == nil {
		t.Error("copy from the end to the start")
	}
	if _, _, err := s.CopyDays(ctx, "2026-01-01", "2026-12-31", 7, "mgr-1"); err == nil {
		t.Error("copy a year")
	}
}

func TestClockingInIsMatchedToTheNearestPlannedShiftAndClockingOutEndsIt(t *testing.T) {
	s, c := newStore(t)
	morning := mustShift(t, s, "an", "2026-11-10", "08:00", "12:00")
	mustShift(t, s, "an", "2026-11-10", "14:00", "18:00")
	c.set("2026-11-10", 8, 10)
	in, err := s.ClockIn(ctx, "an")
	if err != nil || in.RotaID == nil || *in.RotaID != morning.ID || in.ClockOut != nil || in.Day != "2026-11-10" || in.InBy != "an" || in.Manual {
		t.Fatalf("clock in: %+v %v", in, err)
	}
	if _, err := s.ClockIn(ctx, "an"); !errors.Is(err, ErrClockedIn) {
		t.Fatalf("twice: %v", err)
	}
	if open, err := s.OpenEntry(ctx, "an"); err != nil || open.ID != in.ID {
		t.Fatalf("open: %+v %v", open, err)
	}
	if duty, _ := s.OnDuty(ctx); len(duty) != 1 || duty[0].User != "an" {
		t.Fatalf("on duty: %+v", duty)
	}
	c.set("2026-11-10", 12, 40)
	out, err := s.ClockOut(ctx, "an")
	if err != nil || out.ClockOut == nil || out.Minutes != 270 || out.OutBy != "an" {
		t.Fatalf("clock out: %+v %v", out, err)
	}
	if _, err := s.ClockOut(ctx, "an"); !errors.Is(err, ErrNotClockedIn) {
		t.Fatalf("out twice: %v", err)
	}
	if _, err := s.OpenEntry(ctx, "an"); !errors.Is(err, ErrNotClockedIn) {
		t.Fatalf("no open entry: %v", err)
	}
	if duty, _ := s.OnDuty(ctx); len(duty) != 0 {
		t.Fatalf("nobody is on duty: %+v", duty)
	}
	// in again for the afternoon shift, an hour early: it is within three hours
	c.set("2026-11-10", 13, 0)
	in2, err := s.ClockIn(ctx, "an")
	if err != nil || in2.RotaID == nil || *in2.RotaID == morning.ID {
		t.Fatalf("afternoon: %+v %v", in2, err)
	}
}

func TestClockingInWithoutAShiftOrFarFromOneIsAllowedAndUnmatched(t *testing.T) {
	s, c := newStore(t)
	mustShift(t, s, "an", "2026-11-10", "14:00", "18:00")
	c.set("2026-11-10", 9, 0) // five hours early
	in, err := s.ClockIn(ctx, "an")
	if err != nil || in.RotaID != nil {
		t.Fatalf("too early to be matched: %+v %v", in, err)
	}
	c.set("2026-11-10", 9, 30)
	if _, err := s.ClockOut(ctx, "an"); err != nil {
		t.Fatal(err)
	}
	if e, err := s.ClockIn(ctx, "binh"); err != nil || e.RotaID != nil {
		t.Fatalf("no shift at all: %+v %v", e, err)
	}
	// a cancelled shift is not matched
	sh := mustShift(t, s, "chi", "2026-11-10", "08:00", "12:00")
	_, _ = s.CancelShift(ctx, sh.ID, "mgr-1", "")
	if e, _ := s.ClockIn(ctx, "chi"); e.RotaID != nil {
		t.Fatalf("a cancelled shift was matched: %+v", e)
	}
	if _, err := s.ClockIn(ctx, "NOT A LOGIN"); err == nil {
		t.Error("a bad login clocked in")
	}
}

func TestAnEntryStartsOnTheDayOfTheVenueNotOfUTC(t *testing.T) {
	s, c := newStore(t)
	c.set("2026-11-10", 1, 30) // 18:30 the day before in UTC
	in, err := s.ClockIn(ctx, "an")
	if err != nil || in.Day != "2026-11-10" {
		t.Fatalf("day: %+v %v", in, err)
	}
}

func TestAnEntryThatWouldLastMoreThanADayIsNotClosedByAClick(t *testing.T) {
	s, c := newStore(t)
	if _, err := s.ClockIn(ctx, "an"); err != nil {
		t.Fatal(err)
	}
	c.set("2026-11-11", 9, 0) // 26 hours later
	if _, err := s.ClockOut(ctx, "an"); err == nil {
		t.Fatal("a stretch of 26 hours was closed")
	}
	// the manager corrects it
	open, _ := s.OpenEntry(ctx, "an")
	fixed, err := s.CorrectEntry(ctx, open.ID, open.ClockIn, ptr(at("2026-11-10", 16, 0)), "forgot to clock out", "mgr-1")
	if err != nil || fixed.ClockOut == nil || fixed.Minutes != 9*60 || fixed.OutBy != "mgr-1" || fixed.Corrected != 1 {
		t.Fatalf("correct: %+v %v", fixed, err)
	}
	if _, err := s.OpenEntry(ctx, "an"); !errors.Is(err, ErrNotClockedIn) {
		t.Fatal("still open")
	}
}

func ptr(t time.Time) *time.Time { return &t }

func TestAManagerWritesDownForgottenWorkWithAReason(t *testing.T) {
	s, c := newStore(t)
	c.set("2026-11-10", 20, 0)
	e, err := s.AddEntry(ctx, "an", at("2026-11-10", 8, 0), at("2026-11-10", 12, 0), " forgot the phone ", "mgr-1")
	if err != nil || !e.Manual || e.Note != "forgot the phone" || e.Minutes != 240 || e.InBy != "mgr-1" || e.OutBy != "mgr-1" || e.Day != "2026-11-10" {
		t.Fatalf("add: %+v %v", e, err)
	}
	for name, f := range map[string]func() error{
		"no reason": func() error {
			_, err := s.AddEntry(ctx, "an", at("2026-11-10", 13, 0), at("2026-11-10", 14, 0), "  ", "mgr-1")
			return err
		},
		"ends first": func() error {
			_, err := s.AddEntry(ctx, "an", at("2026-11-10", 14, 0), at("2026-11-10", 13, 0), "x", "mgr-1")
			return err
		},
		"over a day": func() error {
			_, err := s.AddEntry(ctx, "an", at("2026-11-08", 8, 0), at("2026-11-09", 12, 0), "x", "mgr-1")
			return err
		},
		"in the future": func() error {
			_, err := s.AddEntry(ctx, "an", at("2026-11-10", 21, 0), at("2026-11-10", 23, 0), "x", "mgr-1")
			return err
		},
		"nobody writes": func() error {
			_, err := s.AddEntry(ctx, "an", at("2026-11-10", 13, 0), at("2026-11-10", 14, 0), "x", "")
			return err
		},
		"bad login": func() error {
			_, err := s.AddEntry(ctx, "", at("2026-11-10", 13, 0), at("2026-11-10", 14, 0), "x", "mgr-1")
			return err
		},
	} {
		if err := f(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := s.AddEntry(ctx, "an", at("2026-11-10", 11, 0), at("2026-11-10", 13, 0), "x", "mgr-1"); !errors.Is(err, ErrOverlap) {
		t.Fatalf("overlap: %v", err)
	}
	// an open stretch blocks anything that is after its start
	if _, err := s.ClockIn(ctx, "binh"); err != nil {
		t.Fatal(err)
	}
	c.set("2026-11-10", 20, 30)
	if _, err := s.AddEntry(ctx, "binh", at("2026-11-10", 19, 0), at("2026-11-10", 20, 15), "x", "mgr-1"); !errors.Is(err, ErrOverlap) {
		t.Fatalf("onto the open stretch: %v", err)
	}
	if _, err := s.AddEntry(ctx, "binh", at("2026-11-10", 6, 0), at("2026-11-10", 6, 30), "x", "mgr-1"); err != nil {
		t.Fatalf("before the open stretch: %v", err)
	}
}

func TestACorrectionKeepsWhatTheEntryWasAndWhy(t *testing.T) {
	s, c := newStore(t)
	c.set("2026-11-10", 20, 0)
	e, err := s.AddEntry(ctx, "an", at("2026-11-10", 8, 0), at("2026-11-10", 12, 0), "forgot", "mgr-1")
	if err != nil {
		t.Fatal(err)
	}
	fixed, err := s.CorrectEntry(ctx, e.ID, at("2026-11-10", 8, 30), ptr(at("2026-11-10", 12, 15)), "the phone says so", "mgr-2")
	if err != nil || fixed.Minutes != 225 || fixed.Corrected != 1 || fixed.InBy != "mgr-1" || fixed.OutBy != "mgr-1" {
		t.Fatalf("fixed: %+v %v", fixed, err)
	}
	again, err := s.CorrectEntry(ctx, e.ID, at("2026-11-10", 8, 30), ptr(at("2026-11-10", 13, 0)), "later", "mgr-1")
	if err != nil || again.Corrected != 2 {
		t.Fatalf("again: %+v %v", again, err)
	}
	h, err := s.History(ctx, e.ID)
	if err != nil || len(h) != 2 {
		t.Fatalf("history: %+v %v", h, err)
	}
	if !h[0].OldClockIn.Equal(at("2026-11-10", 8, 0)) || !h[0].OldOut.Equal(at("2026-11-10", 12, 0)) || !h[0].NewClockIn.Equal(at("2026-11-10", 8, 30)) || h[0].By != "mgr-2" || h[0].Reason != "the phone says so" {
		t.Fatalf("first change: %+v", h[0])
	}
	if !h[1].OldOut.Equal(at("2026-11-10", 12, 15)) || !h[1].NewOut.Equal(at("2026-11-10", 13, 0)) {
		t.Fatalf("second change: %+v", h[1])
	}
	// a correction needs a reason, a sensible span, no overlap, and cannot open what was closed
	for name, f := range map[string]func() error{
		"no reason": func() error {
			_, err := s.CorrectEntry(ctx, e.ID, at("2026-11-10", 8, 0), ptr(at("2026-11-10", 9, 0)), "", "mgr-1")
			return err
		},
		"reversed": func() error {
			_, err := s.CorrectEntry(ctx, e.ID, at("2026-11-10", 9, 0), ptr(at("2026-11-10", 8, 0)), "x", "mgr-1")
			return err
		},
		"reopen": func() error {
			_, err := s.CorrectEntry(ctx, e.ID, at("2026-11-10", 9, 0), nil, "x", "mgr-1")
			return err
		},
		"future": func() error {
			_, err := s.CorrectEntry(ctx, e.ID, at("2026-11-10", 9, 0), ptr(at("2026-11-11", 8, 0)), "x", "mgr-1")
			return err
		},
		"nobody": func() error {
			_, err := s.CorrectEntry(ctx, e.ID, at("2026-11-10", 9, 0), ptr(at("2026-11-10", 10, 0)), "x", "")
			return err
		},
	} {
		if err := f(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := s.CorrectEntry(ctx, 999999, at("2026-11-10", 9, 0), ptr(at("2026-11-10", 10, 0)), "x", "mgr-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no entry: %v", err)
	}
	other, _ := s.AddEntry(ctx, "an", at("2026-11-10", 14, 0), at("2026-11-10", 16, 0), "x", "mgr-1")
	if _, err := s.CorrectEntry(ctx, other.ID, at("2026-11-10", 12, 30), ptr(at("2026-11-10", 16, 0)), "x", "mgr-1"); !errors.Is(err, ErrOverlap) {
		t.Fatalf("a correction onto another: %v", err)
	}
	// a failed correction left no trace
	if h, _ := s.History(ctx, other.ID); len(h) != 0 {
		t.Fatalf("history of a refused correction: %+v", h)
	}
}

func TestTheDatabaseKeepsEntriesAndTheirHistory(t *testing.T) {
	s, c := newStore(t)
	c.set("2026-11-10", 20, 0)
	e, _ := s.AddEntry(ctx, "an", at("2026-11-10", 8, 0), at("2026-11-10", 12, 0), "x", "mgr-1")
	_, _ = s.CorrectEntry(ctx, e.ID, at("2026-11-10", 8, 30), ptr(at("2026-11-10", 12, 0)), "x", "mgr-1")
	for name, q := range map[string]string{
		"delete entry":     `DELETE FROM time_entries`,
		"truncate entries": `TRUNCATE time_entries CASCADE`,
		"reopen":           `UPDATE time_entries SET clock_out = NULL, out_by = NULL`,
		"other person":     `UPDATE time_entries SET user_id = 'binh'`,
		"edit history":     `UPDATE time_entry_history SET reason = 'other'`,
		"delete history":   `DELETE FROM time_entry_history`,
		"truncate history": `TRUNCATE time_entry_history`,
	} {
		if _, err := s.pool.Exec(ctx, q); err == nil {
			t.Errorf("%s: the database let it", name)
		}
	}
	if got, err := s.Get(ctx, e.ID); err != nil || got.Corrected != 1 {
		t.Fatalf("after all that: %+v %v", got, err)
	}
	if _, err := s.Get(ctx, 999999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get nothing: %v", err)
	}
}

func TestHoursSayWhatWasPlannedWhatWasWorkedWhoWasLateAndWhoDidNotCome(t *testing.T) {
	s, c := newStore(t)
	// an: three shifts; on time, 20 minutes late, and one that did not happen
	mustShift(t, s, "an", "2026-11-09", "08:00", "12:00")
	mustShift(t, s, "an", "2026-11-10", "08:00", "12:00")
	mustShift(t, s, "an", "2026-11-11", "08:00", "12:00")
	// binh: one shift, 3 minutes late (within the grace) and worked in two pieces
	mustShift(t, s, "binh", "2026-11-09", "08:00", "12:00")
	// chi: no shift, only worked
	// dung: a cancelled shift counts for nothing
	cancelled := mustShift(t, s, "dung", "2026-11-09", "08:00", "12:00")
	_, _ = s.CancelShift(ctx, cancelled.ID, "mgr-1", "")

	c.set("2026-11-09", 8, 0)
	_, _ = s.ClockIn(ctx, "an")
	c.set("2026-11-09", 12, 0)
	_, _ = s.ClockOut(ctx, "an")
	c.set("2026-11-09", 8, 3)
	_, _ = s.ClockIn(ctx, "binh")
	c.set("2026-11-09", 10, 0)
	_, _ = s.ClockOut(ctx, "binh")
	c.set("2026-11-09", 10, 30)
	_, _ = s.ClockIn(ctx, "binh")
	c.set("2026-11-09", 12, 0)
	_, _ = s.ClockOut(ctx, "binh")
	c.set("2026-11-10", 8, 20)
	_, _ = s.ClockIn(ctx, "an")
	c.set("2026-11-10", 12, 0)
	_, _ = s.ClockOut(ctx, "an")
	c.set("2026-11-10", 9, 0)
	if _, err := s.ClockIn(ctx, "chi"); err != nil {
		t.Fatal(err)
	}
	c.set("2026-11-10", 11, 0)
	_, _ = s.ClockOut(ctx, "chi")
	// eva forgot to clock; a manager wrote the work down, which counts as having come
	mustShift(t, s, "eva", "2026-11-09", "08:00", "12:00")
	if _, err := s.AddEntry(ctx, "eva", at("2026-11-09", 8, 0), at("2026-11-09", 12, 0), "forgot", "mgr-1"); err != nil {
		t.Fatal(err)
	}
	c.set("2026-11-10", 13, 0) // the shift of the 11th is in the future, so it is not missed yet
	_, _ = s.ClockIn(ctx, "dung")

	rows, err := s.Hours(ctx, "2026-11-09", "2026-11-11", "")
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]HoursRow{}
	for _, r := range rows {
		by[r.User] = r
	}
	an := by["an"]
	if an.PlannedShifts != 3 || an.PlannedMin != 720 || an.WorkedMin != 240+220 || an.Days != 2 || an.Late != 1 || an.LateMin != 20 || an.Missed != 0 {
		t.Fatalf("an: %+v", an)
	}
	if b := by["binh"]; b.PlannedShifts != 1 || b.WorkedMin != 117+90 || b.Late != 0 || b.Missed != 0 || b.Days != 1 {
		t.Fatalf("binh: %+v", b)
	}
	if ch := by["chi"]; ch.PlannedShifts != 0 || ch.WorkedMin != 120 || ch.Late != 0 {
		t.Fatalf("chi: %+v", ch)
	}
	if e := by["eva"]; e.PlannedShifts != 1 || e.WorkedMin != 240 || e.Late != 0 || e.Missed != 0 {
		t.Fatalf("eva: %+v (work that a manager wrote down is attendance)", e)
	}
	if d := by["dung"]; d.PlannedShifts != 0 || d.WorkedMin != 0 {
		t.Fatalf("dung: %+v (the entry is still open)", d)
	}
	// the day after: the shift of the 11th has passed and nobody came
	c.set("2026-11-12", 7, 0)
	rows, _ = s.Hours(ctx, "2026-11-09", "2026-11-11", "an")
	if len(rows) != 1 || rows[0].Missed != 1 {
		t.Fatalf("missed: %+v", rows)
	}
	if rows, _ := s.Hours(ctx, "2026-11-09", "2026-11-11", "nobody"); len(rows) != 0 {
		t.Fatalf("nobody: %+v", rows)
	}
}

func TestRatesAreWrittenNotChangedAndTheLatestFromTheDayIsInForce(t *testing.T) {
	s, c := newStore(t)
	for _, in := range []struct {
		user, from string
		rate       int64
	}{{"an", "2026-10-01", 30_000}, {"an", "2026-11-10", 35_000}, {"an", "2026-12-01", 40_000}, {"binh", "2026-10-01", 28_000}} {
		if _, err := s.SetRate(ctx, in.user, in.from, in.rate, "mgr-1"); err != nil {
			t.Fatal(err)
		}
	}
	now, err := s.Rates(ctx, false)
	if err != nil || len(now) != 2 || now[0].User != "an" || now[0].Rate != 35_000 || now[1].Rate != 28_000 {
		t.Fatalf("rates now: %+v %v", now, err)
	}
	all, _ := s.Rates(ctx, true)
	if len(all) != 4 || all[0].User != "an" || all[0].From != "2026-12-01" {
		t.Fatalf("history: %+v", all)
	}
	// writing the same day again: the last written wins
	_, _ = s.SetRate(ctx, "an", "2026-11-10", 36_000, "mgr-2")
	now, _ = s.Rates(ctx, false)
	if now[0].Rate != 36_000 {
		t.Fatalf("the last written: %+v", now)
	}
	_ = c
	for name, f := range map[string]func() error{
		"negative": func() error { _, err := s.SetRate(ctx, "an", "2026-11-10", -1, "mgr-1"); return err },
		"too high": func() error { _, err := s.SetRate(ctx, "an", "2026-11-10", MaxRate+1, "mgr-1"); return err },
		"bad day":  func() error { _, err := s.SetRate(ctx, "an", "soon", 1, "mgr-1"); return err },
		"bad user": func() error { _, err := s.SetRate(ctx, "", "2026-11-10", 1, "mgr-1"); return err },
		"nobody":   func() error { _, err := s.SetRate(ctx, "an", "2026-11-10", 1, ""); return err },
	} {
		if f() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := s.pool.Exec(ctx, `UPDATE hourly_rates SET rate = 1`); err == nil {
		t.Error("a rate was changed")
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM hourly_rates`); err == nil {
		t.Error("a rate was deleted")
	}
}

func TestLabourIsMinutesTimesTheRateOfTheDayRoundedOncePerDay(t *testing.T) {
	s, c := newStore(t)
	c.set("2026-11-20", 23, 0)
	_, _ = s.SetRate(ctx, "an", "2026-11-01", 30_000, "mgr-1")
	_, _ = s.SetRate(ctx, "an", "2026-11-12", 36_000, "mgr-1")
	_, _ = s.SetRate(ctx, "binh", "2026-11-01", 25_000, "mgr-1")
	add := func(user, day string, h1, m1, h2, m2 int) {
		t.Helper()
		if _, err := s.AddEntry(ctx, user, at(day, h1, m1), at(day, h2, m2), "x", "mgr-1"); err != nil {
			t.Fatal(err)
		}
	}
	add("an", "2026-11-10", 8, 0, 12, 0)  // 4 h at 30,000 = 120,000
	add("binh", "2026-11-10", 9, 0, 9, 7) // 7 min at 25,000 = 2,916.67 -> together 122,917 (rounded once)
	add("an", "2026-11-12", 8, 0, 10, 30) // 2.5 h at 36,000 = 90,000 (the new rate)
	add("chi", "2026-11-12", 8, 0, 9, 0)  // no rate: 60 minutes unrated, no cost
	// an entry that is still open costs nothing yet
	c.set("2026-11-13", 8, 0)
	if _, err := s.ClockIn(ctx, "an"); err != nil {
		t.Fatal(err)
	}
	days, err := s.LabourByDay(ctx, "2026-11-01", "2026-11-30")
	if err != nil || len(days) != 2 {
		t.Fatalf("days: %+v %v", days, err)
	}
	if d := days[0]; d.Day != "2026-11-10" || d.Cost != 122_917 || d.Minutes != 247 || d.UnratedMin != 0 {
		t.Fatalf("the 10th: %+v", d)
	}
	if d := days[1]; d.Day != "2026-11-12" || d.Cost != 90_000 || d.Minutes != 210 || d.UnratedMin != 60 {
		t.Fatalf("the 12th: %+v", d)
	}
	if days, _ := s.LabourByDay(ctx, "2026-11-11", "2026-11-11"); len(days) != 0 {
		t.Fatalf("a day with no work: %+v", days)
	}
}

func TestTwoPeopleAddingShiftsAtOnceCannotBothOverlap(t *testing.T) {
	s, _ := newStore(t)
	const n = 8
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := s.AddShift(ctx, ShiftInput{User: "an", Day: "2026-11-10", Start: "08:00", End: "12:00"}, "mgr-1")
			errs <- err
		}()
	}
	won, overlapped := 0, 0
	for i := 0; i < n; i++ {
		switch err := <-errs; {
		case err == nil:
			won++
		case errors.Is(err, ErrOverlap):
			overlapped++
		default:
			t.Fatalf("unexpected: %v", err)
		}
	}
	if won != 1 || overlapped != n-1 {
		t.Fatalf("won %d overlapped %d", won, overlapped)
	}
}

func TestTheMigrationsAreApplied(t *testing.T) {
	pool := pgtest.Pool(t)
	applied, err := pgmigrate.Apply(ctx, pool, Migrations(), pgmigrate.WithTable(MigrationsTable))
	if err != nil || len(applied) == 0 {
		t.Fatalf("apply: %v %v", applied, err)
	}
	again, err := pgmigrate.Apply(ctx, pool, Migrations(), pgmigrate.WithTable(MigrationsTable))
	if err != nil || len(again) != 0 {
		t.Fatalf("apply again: %v %v", again, err)
	}
}

func TestSpanReadsTimesOnTheVenuesClockAndAnEndBeforeTheStartIsTheNextDay(t *testing.T) {
	s, c := newStore(t)
	in, out, err := s.Span("2026-11-10", "08:30", "12:00")
	if err != nil || !in.Equal(at("2026-11-10", 8, 30)) || out == nil || !out.Equal(at("2026-11-10", 12, 0)) {
		t.Fatalf("span: %v %v %v", in, out, err)
	}
	in, out, err = s.Span("2026-11-10", "22:00", "02:15")
	if err != nil || out == nil || !out.Equal(at("2026-11-11", 2, 15)) || out.Sub(in) != 4*time.Hour+15*time.Minute {
		t.Fatalf("over midnight: %v %v %v", in, out, err)
	}
	if _, out, err = s.Span("2026-11-10", "08:30", ""); err != nil || out != nil {
		t.Fatalf("open: %v %v", out, err)
	}
	for _, bad := range [][3]string{{"2026-11-10", "8:30", "12:00"}, {"2026-11-10", "08:30", "25:00"}, {"soon", "08:30", "12:00"}} {
		if _, _, err := s.Span(bad[0], bad[1], bad[2]); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	// the entry says its times on the venue's clock, and the day it ended on if that is another
	c.set("2026-11-12", 12, 0)
	in, out, _ = s.Span("2026-11-10", "22:00", "02:15")
	e, err := s.AddEntry(ctx, "an", in, *out, "forgot", "mgr-1")
	if err != nil || e.In != "22:00" || e.Out != "02:15" || e.OutDay != "2026-11-11" || e.Day != "2026-11-10" || e.Minutes != 255 {
		t.Fatalf("entry: %+v %v", e, err)
	}
	in, out, _ = s.Span("2026-11-10", "08:00", "12:00")
	if e, _ = s.AddEntry(ctx, "binh", in, *out, "forgot", "mgr-1"); e.OutDay != "" {
		t.Fatalf("a same-day entry has no out day: %+v", e)
	}
}
