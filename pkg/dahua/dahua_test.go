package dahua

import "testing"

func TestIsDoorbellPressed(t *testing.T) {
	testCases := []struct {
		name     string
		line     string
		expected bool
	}{
		{
			name:     "valid doorbell press event",
			line:     "Code=AlarmLocal;action=Start;index=0",
			expected: true,
		},
		{
			name:     "valid doorbell press event with extra data",
			line:     "Code=AlarmLocal;action=Start;index=0;data=...",
			expected: true,
		},
		{
			name:     "stop event",
			line:     "Code=AlarmLocal;action=Stop;index=0",
			expected: false,
		},
		{
			name:     "different code",
			line:     "Code=VideoMotion;action=Start;index=0",
			expected: false,
		},
		{
			name:     "malformed line",
			line:     "Code=AlarmLocal;action=Start",
			expected: true,
		},
		{
			name:     "empty line",
			line:     "",
			expected: false,
		},
		{
			name:     "just code",
			line:     "Code=AlarmLocal;",
			expected: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			actual := isDoorbellPressed(tc.line)
			if actual != tc.expected {
				t.Errorf("expected %v, got %v", tc.expected, actual)
			}
		})
	}
}
