#!/usr/bin/env python3
"""MiniFin's icon, in the MiniDeck pixel style: a small TV with a play
button. It borrows the drawing code from MiniDeck's scripts/make-icons.py.

  scripts/make-icon.py ../MiniDeck/scripts/make-icons.py app/icon.png preview.png
"""
import sys
src = open(sys.argv[1]).read().split('if __name__ == "__main__":')[0]
ns = {}
exec(compile(src, "make-icons.py", "exec"), ns)
Art, to_png, sheet = ns["Art"], ns["to_png"], ns["sheet"]

def minifin():
    a = Art()
    a.line(12, 2, 17, 8, "steel", 1)            # antenna
    a.line(23, 2, 18, 8, "steel", 1)
    a.disk(12, 2, 1, "red", flat=True)
    a.rect(2, 8, 33, 30, "lav", r=3)             # the set
    a.rect(5, 11, 25, 27, "night", r=2, flat=True)  # screen
    for i in range(8):                           # play triangle
        a.rect(11 + i, 12 + i * 3 // 4, 11 + i, 26 - i * 3 // 4 - (1 if i == 7 else 0), "white", flat=True)
    a.disk(29, 15, 2, "orange")                  # knobs
    a.disk(29, 22, 2, "sky")
    a.rect(6, 31, 9, 32, "steel")                # feet
    a.rect(26, 31, 29, 32, "steel")
    return a

g = minifin().finish()
open(sys.argv[2], "wb").write(to_png(g, 2, 74))
open(sys.argv[3], "wb").write(sheet([g], 6, (22, 24, 40)))
