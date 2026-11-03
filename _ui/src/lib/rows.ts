// rowClick opens to when a table row is clicked, but not when the click was
// on a link, a button or a form control inside it, on selected text, or with
// a modifier (the link in the row handles opening in a new tab).
export function rowClick(to: () => void) {
  return (e: MouseEvent) => {
    if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return
    const target = e.target as HTMLElement
    if (target.closest('a, button, input, select, textarea, label')) return
    if (window.getSelection()?.toString()) return
    to()
  }
}
