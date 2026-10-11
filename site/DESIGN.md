# rackhands design brief

Reference object: a bank safe-deposit vault. A wall of numbered brass box doors, each with two keyholes (the bank's guard key and the renter's key), plus the access register you sign before a box is opened.
Metaphor: the wall is your rack. Each door is one server and its BMC. Looking at a door is free: the little window shows its readings. Opening one is a dangerous operation. It needs the vault key (runtime `--allow`), the renter's key (the target's `allow:`), and a signature in the register (confirmation, written to the audit log). You open one box at a time.

Registered in `../../design/registry.md` (2026-10-12) before implementation. I dropped the earlier railway-interlocking idea because postsleuth already owns a rail/metro language.

- Base: mid-tone saturated brushed brass #c79a3b fills the hero and the body. Brass shade #9c7426, brass highlight #e3c474, bronze ink #21180e (text, keyholes, footer), wine velvet #5c1a24 (only inside an opened box), steel #a9b0b3 (bolts, hinges), enamel number-plate white #eef0ea.
- Type: Gloock (high-contrast engraved serif; plates and headings only), Hanken Grotesk 400/600 (body), Martian Mono 400 (commands and plan output only). Fonts are self-hosted woff2 under OFL.
- Texture: very faint horizontal brushed-metal hairlines and engraved lettering (a 1px light/dark inset edge, not a drop shadow), plus rivets. No paper, no grain, no hard shadows, no glow, no gradient colour fields, no gradient text.
- Rendering: CSS 3D. Each door has a perspective box and swings on a hinge with rotateY. Keys rotate with rotateZ. Drawers slide out with translateZ and rotateX. Everything is plain HTML/CSS elements; no canvas and no generated SVG scene.
- Layout:
  - The hero is a full-bleed grid of box doors, 6 columns on desktop and 2 on mobile. Each door has an enamel number plate (the target name), two keyholes, and a reading window.
  - The h1 is engraved on a wide plate in the middle of the wall. It is not a free-floating headline.
  - Each section below is a long drawer pulled out of the wall. Its front plate has a handle and a box number, No. 101 to 106.
  - Drawer No. 102 is the interactive "open one box" panel: two keys, a register to sign, and a door that opens onto the plan.
- Motion:
  - Reading windows on the wall flip (rotateX) every few seconds. Reads need no key.
  - In the interactive box, turning one key alone jams: the bolt stays and the CLI refusal is shown. With both keys turned, the bolt slides back. The door only swings open after the target name is typed into the register. Inside, on velvet, are the PLAN and DONE lines.
  - Drawers slide out once as they scroll into view.
  - `prefers-reduced-motion`: there are no transitions or flips. States change instantly, and drawers are already out.
- Mobile: the wall becomes 2 columns, with the title plate at full width first. Drawers are full width. The key buttons are big enough to tap (at least 44px).
- Logo: a horizontal 2:1 1U rack faceplate with square corners and a mounting-screw hole at each end. Two keyholes sit side by side on it. Brass face, bronze-ink keyholes, transparent background. It is not a rounded-square app tile.
- OG and social preview: the same wall, cropped. The title plate carries "rackhands" and one line of text, and one door stands open on velvet.
- Performance budget: under 60 KB of JS+CSS, fonts preloaded, no layout shift (doors have a fixed aspect ratio), Lighthouse 90 or better on all four categories.
