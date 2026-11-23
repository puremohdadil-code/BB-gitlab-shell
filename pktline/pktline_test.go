package pktline

import (
	"bytes"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const pktlineHelloWorld = "0010hello world!"

var (
	largestString = strings.Repeat("z", 0xffff-4)
)

func TestScanner(t *testing.T) {
	largestPacket := "ffff" + largestString
	testCases := []struct {
		desc string
		in   string
		out  []string
		fail bool
	}{
		{
			desc: "happy path",
			in:   "0010hello world!00000001000200030010hello world!",
			out:  []string{pktlineHelloWorld, "0000", "0001", "0002", "0003", pktlineHelloWorld},
		},
		{
			desc: "large input",
			in:   "0010hello world!0000" + largestPacket + "0000",
			out:  []string{pktlineHelloWorld, "0000", largestPacket, "0000"},
		},
		{
			desc: "missing byte middle",
			in:   "0010hello world!00000010010hello world!",
			out:  []string{pktlineHelloWorld, "0000", "0010010hello wor"},
			fail: true,
		},
		{
			desc: "unfinished prefix",
			in:   "0010hello world!000",
			out:  []string{pktlineHelloWorld},
			fail: true,
		},
		{
			desc: "short read in data, only prefix",
			in:   "0010hello world!0005",
			out:  []string{pktlineHelloWorld},
			fail: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			scanner := NewScanner(strings.NewReader(tc.in))
			var output []string
			for scanner.Scan() {
				output = append(output, scanner.Text())
			}

			if tc.fail {
				require.Error(t, scanner.Err())
			} else {
				require.NoError(t, scanner.Err())
			}

			require.Equal(t, tc.out, output)
		})
	}
}

func TestReadPacket(t *testing.T) {
	testCases := []struct {
		desc    string
		input   string
		want    string
		wantErr error
	}{
		{desc: "flush packet", input: "0000trailing", want: "0000"},
		{desc: "delimiter packet", input: "0001trailing", want: "0001"},
		{desc: "response end packet", input: "0002trailing", want: "0002"},
		{desc: "reserved packet", input: "0003trailing", want: "0003"},
		{desc: "data packet", input: "000Ahello\ntrailing", want: "000Ahello\n"},
		{desc: "empty input", wantErr: io.EOF},
		{desc: "incomplete prefix", input: "000", wantErr: io.ErrUnexpectedEOF},
		{desc: "non-hexadecimal prefix", input: "zzzz", wantErr: strconv.ErrSyntax},
		{desc: "length prefix only", input: "0008", wantErr: io.ErrUnexpectedEOF},
		{desc: "incomplete data", input: "0008abc", wantErr: io.ErrUnexpectedEOF},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			reader := bytes.NewBufferString(tc.input)

			packet, err := ReadPacket(reader)

			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, []byte(tc.want), packet)
			require.Equal(t, strings.TrimPrefix(tc.input, tc.want), reader.String())
		})
	}
}

func TestIsRefRemoval(t *testing.T) {
	testCases := []struct {
		in        string
		isRemoval bool
	}{
		{in: "003f7217a7c7e582c46cec22a130adf4b9d7d950fba0 7d1665144a3a975c05f1f43902ddaf084e784dbe refs/heads/debug", isRemoval: false},
		{in: "003f0000000000000000000000000000000000000000 7d1665144a3a975c05f1f43902ddaf084e784dbe refs/heads/debug", isRemoval: false},
		{in: "003f7217a7c7e582c46cec22a130adf4b9d7d950fba0 0000000000000000000000000000000000000000 refs/heads/debug", isRemoval: true},
		{in: "0096" + strings.Repeat("a", 64) + " " + strings.Repeat("0", 64) + " refs/heads/debug", isRemoval: true},
		{in: "0096" + strings.Repeat("0", 64) + " " + strings.Repeat("b", 64) + " refs/heads/debug", isRemoval: false},
		{in: "0096" + strings.Repeat("a", 64) + " " + strings.Repeat("b", 64) + " refs/heads/debug", isRemoval: false},
		{in: "007e" + strings.Repeat("a", 40) + " " + strings.Repeat("0", 64) + " refs/heads/debug", isRemoval: false},
		{in: "007e" + strings.Repeat("a", 64) + " " + strings.Repeat("0", 40) + " refs/heads/debug", isRemoval: false},
	}

	for _, tc := range testCases {
		t.Run(tc.in, func(t *testing.T) {
			require.Equal(t, tc.isRemoval, IsRefRemoval([]byte(tc.in)))
		})
	}
}

func TestIsShallow(t *testing.T) {
	testCases := []struct {
		in      string
		shallow bool
	}{
		{in: "0035shallow " + strings.Repeat("a", 40) + "\n", shallow: true},
		{in: "004dshallow " + strings.Repeat("a", 64) + "\n", shallow: true},
		{in: "", shallow: false},
		{in: "000bshallow", shallow: false},
		{in: "003f7217a7c7e582c46cec22a130adf4b9d7d950fba0 7d1665144a3a975c05f1f43902ddaf084e784dbe refs/heads/debug", shallow: false},
	}

	for _, tc := range testCases {
		t.Run(tc.in, func(t *testing.T) {
			require.Equal(t, tc.shallow, IsShallow([]byte(tc.in)))
		})
	}
}

func TestIsFlush(t *testing.T) {
	testCases := []struct {
		in    string
		flush bool
	}{
		{in: "0008abcd", flush: false},
		{in: "invalid packet", flush: false},
		{in: "0000", flush: true},
	}

	for _, tc := range testCases {
		t.Run(tc.in, func(t *testing.T) {
			require.Equal(t, tc.flush, IsFlush([]byte(tc.in)))
		})
	}
}

func TestIsDone(t *testing.T) {
	testCases := []struct {
		in   string
		done bool
	}{
		{in: "0008abcd", done: false},
		{in: "invalid packet", done: false},
		{in: "0009done\n", done: true},
		{in: "0001", done: false},
	}

	for _, tc := range testCases {
		t.Run(tc.in, func(t *testing.T) {
			require.Equal(t, tc.done, IsDone([]byte(tc.in)))
		})
	}
}
