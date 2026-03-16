# REPL command namespaces and shell-level aliases

## Summary

The REPL is growing into a small language rather than a thin wrapper
around the bpfman CLI.

That creates a namespace problem.

Today, the REPL exposes two kinds of commands in one flat top-level
space:

* **shell-language commands**, such as:

  * `let`
  * `set`
  * `assert`
  * `require`
  * `exec`
  * `json`
  * `dump`
  * `vars`
  * `unset`
  * `source`
* **bpfman domain commands**, such as:

  * `program`
  * `link`
  * `dispatcher`
  * `doctor`
  * `gc`
  * future commands like `map`

This works early on, but it risks future collisions between:

* REPL language keywords
* bpfman domain nouns and verbs

Examples of plausible future shell-language words include:

* `map`
* `file`
* `value`
* `diff`
* `alias`

Examples of plausible future domain commands include:

* `map get`
* `map list`
* `map dump`

If both live in one flat namespace, language growth and domain growth
start to interfere with each other.

The generic solution is to introduce an explicit **domain namespace**
inside the REPL, and to treat convenience shorthands as a **shell
feature**, not as part of the domain grammar itself.

## Problem

### Flat namespaces do not scale well

A flat top-level command space is attractive at first:

```bpfman
program get 123
link attach xdp -i eth0 $prog
assert $count > 0
json parse $raw.stdout
```

But as the REPL language grows, this makes every new shell feature
compete with every existing and future bpfman command name.

For example:

* if the shell later wants a `map` construct, that collides with a
  natural domain command `map`
* if the shell adds `file temp`, that competes with any future domain
  use of `file`
* if the shell adds `value diff`, that competes with domain nouns like
  `value` if they ever arise

This is not merely cosmetic. It affects the long-term shape of the
language.

### The REPL is not just the CLI

The REPL is already growing language-specific features that do not map
directly to the CLI:

* `let`
* `set`
* `assert`
* `require`
* `json parse`
* `exec`
* structured values and path access
* future features like `file temp`, `value keys`, infix assertions,
  and so on

This means the REPL is not simply "the CLI, but interactive". It is a
small language with an embedded domain API.

Once that is true, it is worth making the boundary explicit.

## Design goal

The REPL should distinguish clearly between:

* **shell-language commands**
* **embedded bpfman domain commands**

That separation should:

* avoid future namespace collisions
* make the grammar easier to extend
* keep the shell language small and curated
* preserve good interactive ergonomics

## Generic solution

Introduce an explicit **domain prefix** in the REPL.

Examples:

```bpfman
bpfman program get 123
bpfman link attach xdp -i eth0 $prog
bpfman map get 42
```

Under this model:

* shell-language commands remain bare
* domain commands are always introduced through a namespace marker

This cleanly separates:

### Shell language

```bpfman
let raw = exec bpftool map dump id $map_id
let data = json parse $raw.stdout
assert $count > 0
dump data[0]
```

### Domain commands

```bpfman
let prog = bpfman program get 123
let link = bpfman link attach xdp -i veth-host $prog
bpfman program unload $pid
```

The key point is that the **namespace boundary belongs to the shell**,
not to the domain model. The domain model stays the same; the shell
decides how that model is introduced.

## Why this helps

### 1. Future shell features get room to grow

Words like:

* `map`
* `file`
* `value`
* `alias`
* `diff`

can be reserved for shell-language features without fear of colliding
with domain nouns.

### 2. Future domain commands get room to grow

The domain command surface can expand naturally under the namespace:

```bpfman
bpfman map get ...
bpfman map list ...
bpfman map dump ...
```

without competing with shell syntax.

### 3. The REPL grammar becomes clearer

The language reads more like:

* a small shell
* plus one embedded domain namespace

rather than one flat "bag of commands".

## Ergonomics concern

The downside is obvious: requiring the full `bpfman` keyword in the
interactive REPL is slightly clumsy.

For example:

```bpfman
bpfman program get 123
bpfman link list
bpfman map get 42
```

is heavier than:

```bpfman
program get 123
link list
map get 42
```

This is a real usability cost, especially in interactive exploration.

However:

* tab completion reduces the burden
* history reduces the burden
* the structural clarity may still be worth it

Even so, there is a better ergonomic answer than returning to a flat
namespace.

## Shell-level shorthand

If the explicit domain prefix is the correct **grammar**, then short
interactive forms should be handled by the **shell**, not by weakening
the grammar.

That suggests a generic shell-level alias mechanism.

For example:

```bpfman
alias b = bpfman
b program get 123
b link attach xdp -i veth-host $prog
```

This gives:

* a clean canonical namespace boundary
* shorter interactive spelling
* no need to hard-code one special shorthand forever

The important design point is that:

* `bpfman` remains the canonical domain namespace
* `b` is a shell alias chosen for convenience

That keeps the architecture clean.

## Why alias is the right layer

Because the problem is not really a domain problem. It is a shell
ergonomics problem.

The shell should own conveniences like:

* short names
* custom shorthands
* interactive abbreviations

The embedded domain should remain explicit and stable.

In other words:

* **namespace design** belongs to the language
* **typing convenience** belongs to the shell

That is a healthier separation than building ad hoc domain shortcuts
into the core grammar.

## Narrow alias model

If an alias feature is added, it should stay intentionally small.

A minimal model would be:

* aliases rewrite only the **first token**
* expansion is non-recursive
* aliases are session-local
* aliases are optional convenience, not semantic machinery

Examples:

```bpfman
alias b = bpfman
alias bp = bpfman
```

This would allow:

```bpfman
b program get 123
bp link list
```

without turning the REPL into a full shell alias system.

## Alternatives considered

### 1. Keep the flat namespace

This is simplest now, but risks collisions as the language grows.

### 2. Require full `bpfman` everywhere, no shorthand

Architecturally clean, but slightly clumsy for interactive use.

### 3. Hard-code a single short prefix such as `b`

This is workable, but mixes shell ergonomics into the core grammar.
It is less flexible than a shell-level alias.

### 4. Allow both bare domain commands and namespaced ones

This weakens the main benefit of introducing the namespace in the first
place. Collisions remain a risk.

## Recommendation

Adopt the following model:

### 1. The REPL language has an explicit domain namespace

Canonical form:

```bpfman
bpfman program get ...
bpfman link attach ...
bpfman map get ...
```

### 2. Shell-language commands remain bare

Examples:

```bpfman
let
set
assert
require
exec
json
dump
file
value
alias
```

### 3. Interactive shorthand is handled by the shell

For example:

```bpfman
alias b = bpfman
```

so users can write:

```bpfman
b program get 123
```

This keeps the grammar clean while preserving interactive convenience.

## Bottom line

The problem is not "how do we type less in the REPL?"

The real problem is:

**how do we stop the shell language and the embedded domain API from
tripping over each other as both grow?**

The generic answer is:

* give the domain a namespace
* give the shell an alias mechanism for convenience

That keeps the language extensible without making interactive use
painful.
