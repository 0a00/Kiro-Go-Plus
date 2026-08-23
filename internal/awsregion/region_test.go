package awsregion

import "testing"

func TestNormalize(t *testing.T) {
	for _, test := range []struct {
		input string
		want  string
	}{
		{input: "us-east-1", want: "us-east-1"},
		{input: " EU-NORTH-1 ", want: "eu-north-1"},
		{input: "us-gov-west-1", want: "us-gov-west-1"},
	} {
		got, err := Normalize(test.input)
		if err != nil || got != test.want {
			t.Fatalf("Normalize(%q) = %q, %v; want %q", test.input, got, err, test.want)
		}
	}
}

func TestNormalizeRejectsHostInjection(t *testing.T) {
	for _, input := range []string{
		"attacker.example#", "us-east-1@attacker.example", "us-east-1/path",
		"us-east-1?target=attacker", "us-east-1:443", "-invalid-1", "invalid-",
	} {
		if got, err := Normalize(input); err == nil {
			t.Fatalf("Normalize(%q) = %q, want rejection", input, got)
		}
	}
}
