// Package clipboard writes bytes to the system clipboard. It is platform-split:
// macOS shells out to pbcopy, and every other platform uses a stub until a real
// integration lands. Callers get one Copy function regardless of platform.
package clipboard
