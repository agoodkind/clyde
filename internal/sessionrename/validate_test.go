package sessionrename

import (
	"errors"
	"testing"
)

func TestValidateAcceptsKebab(t *testing.T) {
	got, err := Validate(ValidationInput{Candidate: "auth-login-flow"})
	if err != nil {
		t.Fatalf("Validate accept kebab: unexpected err %v", err)
	}
	if got != "auth-login-flow" {
		t.Fatalf("Validate accept kebab: got %q, want %q", got, "auth-login-flow")
	}
}

func TestValidateRejectsTooShort(t *testing.T) {
	_, err := Validate(ValidationInput{Candidate: "abc"})
	if !errors.Is(err, ErrCandidateShape) {
		t.Fatalf("Validate too short: got %v, want %v", err, ErrCandidateShape)
	}
}

func TestValidateRejectsTooLong(t *testing.T) {
	long := "a" + string(make([]byte, 49))
	for index := range long[1:] {
		long = long[:index+1] + "a" + long[index+2:]
	}
	_, err := Validate(ValidationInput{Candidate: long + "extra"})
	if !errors.Is(err, ErrCandidateShape) {
		t.Fatalf("Validate too long: got %v, want %v", err, ErrCandidateShape)
	}
}

func TestValidateRejectsUUID(t *testing.T) {
	candidate := "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d"
	_, err := Validate(ValidationInput{Candidate: candidate})
	if !errors.Is(err, ErrCandidateUUID) && !errors.Is(err, ErrCandidateShape) {
		t.Fatalf("Validate UUID: got %v, want UUID or shape rejection", err)
	}
}

func TestValidateRejectsPath(t *testing.T) {
	_, err := Validate(ValidationInput{Candidate: "auth/login-flow"})
	if !errors.Is(err, ErrCandidatePath) {
		t.Fatalf("Validate path: got %v, want %v", err, ErrCandidatePath)
	}
}

func TestValidateRejectsProviderID(t *testing.T) {
	candidate := "abc-def-ghi-jkl"
	_, err := Validate(ValidationInput{
		Candidate:         candidate,
		ProviderSessionID: candidate,
	})
	if !errors.Is(err, ErrCandidateProviderID) {
		t.Fatalf("Validate provider id: got %v, want %v", err, ErrCandidateProviderID)
	}
}

func TestValidateRejectsUserOwnedCollision(t *testing.T) {
	_, err := Validate(ValidationInput{
		Candidate: "billing-overhaul",
		ExistingNames: []ExistingNameOwner{
			{Name: "billing-overhaul", UserOwned: true},
		},
	})
	if !errors.Is(err, ErrCandidateUserOwned) {
		t.Fatalf("Validate user-owned: got %v, want %v", err, ErrCandidateUserOwned)
	}
}

func TestValidateAppendsSuffixOnCollision(t *testing.T) {
	got, err := Validate(ValidationInput{
		Candidate: "billing-overhaul",
		ExistingNames: []ExistingNameOwner{
			{Name: "billing-overhaul", UserOwned: false},
		},
	})
	if err != nil {
		t.Fatalf("Validate suffix: unexpected err %v", err)
	}
	if got != "billing-overhaul-2" {
		t.Fatalf("Validate suffix: got %q, want %q", got, "billing-overhaul-2")
	}
}

func TestValidateExhaustsSuffixes(t *testing.T) {
	existing := []ExistingNameOwner{{Name: "name-foo", UserOwned: false}}
	for suffix := 2; suffix <= 9; suffix++ {
		existing = append(existing, ExistingNameOwner{Name: "name-foo-" + string(rune('0'+suffix)), UserOwned: false})
	}
	_, err := Validate(ValidationInput{
		Candidate:     "name-foo",
		ExistingNames: existing,
	})
	if !errors.Is(err, ErrCandidateExhausted) {
		t.Fatalf("Validate exhausted: got %v, want %v", err, ErrCandidateExhausted)
	}
}

func TestValidateRejectsEmpty(t *testing.T) {
	_, err := Validate(ValidationInput{Candidate: "   "})
	if !errors.Is(err, ErrCandidateEmpty) {
		t.Fatalf("Validate empty: got %v, want %v", err, ErrCandidateEmpty)
	}
}

func TestValidateRejectsLeadingHyphen(t *testing.T) {
	_, err := Validate(ValidationInput{Candidate: "-leading-hyphen"})
	if !errors.Is(err, ErrCandidateShape) {
		t.Fatalf("Validate leading hyphen: got %v, want %v", err, ErrCandidateShape)
	}
}

func TestValidateRejectsTrailingHyphen(t *testing.T) {
	_, err := Validate(ValidationInput{Candidate: "trailing-hyphen-"})
	if !errors.Is(err, ErrCandidateShape) {
		t.Fatalf("Validate trailing hyphen: got %v, want %v", err, ErrCandidateShape)
	}
}

func TestValidateRejectsUppercase(t *testing.T) {
	_, err := Validate(ValidationInput{Candidate: "Mixed-Case"})
	if !errors.Is(err, ErrCandidateShape) {
		t.Fatalf("Validate uppercase: got %v, want %v", err, ErrCandidateShape)
	}
}
