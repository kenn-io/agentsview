package friction

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCode(t *testing.T) {
	for _, tt := range []struct {
		name string
		text string
		want string
	}{
		{"empty", "", ""},
		{"embedded backticks", "use `pwd` now", "``use `pwd` now``"},
		{"boundary backticks", "`Bash` x3 identical arguments", "`` `Bash` x3 identical arguments ``"},
		{"trailing backtick", "run `pwd`", "`` run `pwd` ``"},
		{"longest run", "a``b```c", "````a``b```c````"},
		{"boundary spaces", "  text  ", "`   text   `"},
		{"leading space", " text", "` text`"},
		{"only spaces", "   ", "`   `"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, code(tt.text))
		})
	}
}
