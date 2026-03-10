-- Schema for bpfman SQLite database
--
-- This schema uses the registry + detail tables pattern for links,
-- providing both polymorphic access and type-specific constraints.
--
-- Entity lifecycle
-- ================
--
-- Programs (managed_programs)
-- ---------------------------
--
-- CREATE: A row is inserted only after a BPF program has been
--   successfully loaded into the kernel. There are no intermediate
--   reservation or loading states. The program_id is the
--   kernel-assigned BPF program ID.
--
-- UPDATE: Programs may be updated to change metadata, ownership, or
--   map relationships. The map_owner_id self-reference allows
--   multiple programs to share BPF maps with one designated owner.
--
-- DELETE: Deleting a program cascades through the entire link
--   hierarchy:
--
--     DELETE managed_programs
--       -> CASCADE to links (via kernel_prog_id FK)
--         -> CASCADE to link_*_details (via link_id FK)
--
--   A single DELETE on managed_programs cleans up the base link row
--   and its type-specific detail row automatically.
--
--   Exception: if the program is a map owner (another program's
--   map_owner_id points to it), the delete is RESTRICTED. You must
--   delete the dependent programs first.
--
-- Links (links + link_*_details)
-- ------------------------------
--
-- The links table is a polymorphic registry. Every link gets a row
-- here regardless of type, with a "kind" discriminator column that
-- indicates which detail table holds the type-specific data. Each
-- detail table has a 1:1 relationship with links, joined on link_id.
-- This avoids a single wide nullable table and lets each type enforce
-- its own constraints.
--
-- CREATE: A link row is inserted into both the base links table and
--   the appropriate detail table in a single transaction. The link_id
--   is either kernel-assigned (for real BPF links) or
--   bpfman-assigned in the synthetic range (>= 0x80000000) for
--   attach types like perf_event where the kernel does not provide a
--   link FD/ID.
--
-- UPDATE: Detail rows may be updated (e.g., to change dispatcher
--   position or revision during a dispatcher recompile). The base
--   links row is generally immutable after creation.
--
-- DELETE: Deleting a link row cascades to its detail table row.
--   Links are also deleted automatically when their parent program is
--   deleted (see program deletion above).
--
-- Dispatchers (dispatchers)
-- -------------------------
--
-- XDP and TC do not natively support multiple programs on one
-- interface, so bpfman uses dispatcher BPF programs to chain them.
-- There is exactly one dispatcher per (type, nsid, ifindex) tuple.
--
-- CREATE: A dispatcher row is inserted when the first extension
--   program is attached to an interface. The dispatcher's program_id
--   is the kernel-assigned ID of the dispatcher BPF program itself.
--   There is deliberately no FK back to managed_programs, giving
--   flexibility in lifecycle ordering (the dispatcher row may be
--   created before or after the corresponding managed_programs row).
--
-- UPDATE: When a dispatcher is recompiled (e.g., extensions
--   added/removed), its revision is incremented and its program_id
--   may change to reflect the new kernel program. Because
--   link_xdp_details and link_tc_details reference
--   dispatchers(program_id) with ON UPDATE CASCADE, all extension
--   link rows are automatically updated to track the new
--   program_id. Without this, every extension link row would need
--   manual updating after a dispatcher recompile.
--
-- DELETE: Removing a dispatcher cascades to the extension link detail
--   rows (link_xdp_details, link_tc_details) that reference it, via
--   ON DELETE CASCADE on the dispatcher_program_id FK. Those
--   extensions cannot run without their dispatcher, so cleanup is
--   appropriate. Note that this cascade removes only the detail rows;
--   the parent links and managed_programs rows are unaffected.
--
-- TCX (link_tcx_details)
-- ----------------------
--
-- TCX is a special case: the kernel handles multi-program ordering
-- natively, so no dispatcher is needed. TCX detail rows have no
-- dispatcher_program_id, no position column, and no dispatcher
-- cascade behaviour. They are cleaned up solely by the links cascade.
--
-- Foreign key actions reference
-- =============================
--
-- ON DELETE CASCADE: when the referenced (parent) row is deleted,
--   automatically delete all rows that reference it. Used here so
--   that deleting a program removes its links, and deleting a link
--   removes its detail row. The cascade can chain: deleting a
--   managed_programs row cascades to links, which cascades to
--   link_*_details.
--
-- ON DELETE RESTRICT: prevent the delete entirely if any row still
--   references the target. The delete statement fails with an error.
--   Used on map_owner_id so that a map-owning program cannot be
--   removed while dependent programs still exist.
--
-- ON UPDATE CASCADE: when the referenced column value in the parent
--   row changes, automatically update the FK column in all
--   referencing rows to match. Used on dispatcher_program_id so that
--   when a dispatcher is recompiled and receives a new program_id,
--   all extension link rows are updated without manual intervention.
--
-- STRICT: a SQLite table mode that enforces column types. Without
--   it, SQLite allows any value in any column regardless of declared
--   type. With STRICT, inserting a TEXT into an INTEGER column (or
--   vice versa) is an error. Every table in this schema uses STRICT.
--
-- CHECK: an inline constraint that validates a value at
--   insert/update time. Used throughout for enum-style columns
--   (program_type, kind, direction), range constraints (offset >= 0,
--   position BETWEEN 0 AND 9), boolean columns (IN (0, 1)), and
--   JSON validation (json_valid).

-- Programs table for managed BPF programs
-- A row exists only after successful load - no reservation/loading states.
-- Schema is normalised: individual columns for queryable fields, JSON only for opaque data.
CREATE TABLE IF NOT EXISTS managed_programs (
    program_id INTEGER PRIMARY KEY,
    program_name TEXT NOT NULL,
    program_type TEXT NOT NULL CHECK (program_type IN (
        'xdp','tc','tcx','tracepoint','kprobe','kretprobe',
        'uprobe','uretprobe','fentry','fexit'
    )),
    object_path TEXT NOT NULL,
    pin_path TEXT NOT NULL,
    attach_func TEXT,
    global_data TEXT CHECK (global_data IS NULL OR json_valid(global_data)),
                                     -- JSON map<string, bytes>, opaque
    map_owner_id INTEGER,        -- Self-reference: program that owns shared maps
    map_pin_path TEXT,           -- Directory where maps are pinned
    image_source TEXT CHECK (
        image_source IS NULL
        OR (
            json_valid(image_source)
            AND json_extract(image_source, '$.url') IS NOT NULL
            AND json_extract(image_source, '$.url') != ''
            AND json_extract(image_source, '$.pull_policy') IN (
                'Always', 'IfNotPresent', 'Never'
            )
        )
    ),           -- JSON ImageSource struct, NULL if file-loaded
    owner TEXT,
    description TEXT,
    license TEXT,                -- ELF license string from bytecode
    gpl_compatible INTEGER NOT NULL DEFAULT 0 CHECK (gpl_compatible IN (0, 1)),
    metadata_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(metadata_json)),
                                     -- User key-value metadata as JSON
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,

    -- Self-referential: when multiple programs share BPF maps, one is
    -- designated the owner. RESTRICT prevents deleting the owner
    -- while any dependent program still references it.
    FOREIGN KEY (map_owner_id)
        REFERENCES managed_programs(program_id)
        ON DELETE RESTRICT
) STRICT;

-- Note: No uniqueness constraint on bpfman.io/ProgramName.
-- Multiple programs can share the same application name (e.g., when loading
-- multiple BPF programs from a single image via the operator).

--------------------------------------------------------------------------------
-- Links Table (Polymorphic Core)
--------------------------------------------------------------------------------

-- links contains all common fields for managed links.
-- link_id is the primary key: kernel-assigned for real BPF links,
-- or bpfman-assigned (0x80000000+) for synthetic/perf_event links.
-- This matches the ID users see in CLI and bpftool.
CREATE TABLE IF NOT EXISTS links (
    link_id         INTEGER PRIMARY KEY,  -- kernel ID or synthetic ID
    kind            TEXT NOT NULL CHECK (kind IN (
                        'tracepoint','kprobe','kretprobe','uprobe','uretprobe',
                        'fentry','fexit','xdp','tc','tcx'
                    )),        -- LinkKind discriminator
    kernel_prog_id  INTEGER NOT NULL,     -- useful for queries
    pin_path        TEXT,
    is_synthetic    INTEGER NOT NULL DEFAULT 0 CHECK (is_synthetic IN (0, 1)),
    created_at      TEXT NOT NULL,

    -- Enforce synthetic ID range: synthetic links must have ID >= 0x80000000
    CHECK (
        (is_synthetic = 1 AND link_id >= 2147483648)
        OR
        (is_synthetic = 0 AND link_id < 2147483648)
    ),

    -- Deleting a program cascades here, removing all its links.
    -- This in turn cascades to the type-specific detail tables.
    FOREIGN KEY (kernel_prog_id)
        REFERENCES managed_programs(program_id)
        ON DELETE CASCADE
) STRICT;

CREATE INDEX IF NOT EXISTS idx_links_by_prog ON links(kernel_prog_id);
CREATE INDEX IF NOT EXISTS idx_links_by_kind ON links(kind);
CREATE INDEX IF NOT EXISTS idx_links_by_pin ON links(pin_path);

--------------------------------------------------------------------------------
-- Type-Specific Detail Tables
--------------------------------------------------------------------------------
-- Each link kind has a 1:1 detail table joined on link_id. This avoids a
-- single wide nullable table; each detail table contains only the columns
-- relevant to its type. All detail tables cascade on delete from links,
-- which in turn cascades from managed_programs.

-- Tracepoint links
CREATE TABLE IF NOT EXISTS link_tracepoint_details (
    link_id INTEGER PRIMARY KEY,
    tp_group TEXT NOT NULL,
    tp_name TEXT NOT NULL,

    FOREIGN KEY (link_id)
        REFERENCES links(link_id)
        ON DELETE CASCADE
) STRICT;

-- Kprobe/Kretprobe links
CREATE TABLE IF NOT EXISTS link_kprobe_details (
    link_id INTEGER PRIMARY KEY,
    fn_name TEXT NOT NULL,
    offset INTEGER NOT NULL DEFAULT 0 CHECK (offset >= 0),
    retprobe INTEGER NOT NULL DEFAULT 0 CHECK (retprobe IN (0, 1)),

    FOREIGN KEY (link_id)
        REFERENCES links(link_id)
        ON DELETE CASCADE
) STRICT;

-- Uprobe/Uretprobe links
CREATE TABLE IF NOT EXISTS link_uprobe_details (
    link_id INTEGER PRIMARY KEY,
    target TEXT NOT NULL,
    fn_name TEXT,
    offset INTEGER NOT NULL DEFAULT 0 CHECK (offset >= 0),
    pid INTEGER,
    retprobe INTEGER NOT NULL DEFAULT 0 CHECK (retprobe IN (0, 1)),

    FOREIGN KEY (link_id)
        REFERENCES links(link_id)
        ON DELETE CASCADE
) STRICT;

-- Fentry links
CREATE TABLE IF NOT EXISTS link_fentry_details (
    link_id INTEGER PRIMARY KEY,
    fn_name TEXT NOT NULL,

    FOREIGN KEY (link_id)
        REFERENCES links(link_id)
        ON DELETE CASCADE
) STRICT;

-- Fexit links
CREATE TABLE IF NOT EXISTS link_fexit_details (
    link_id INTEGER PRIMARY KEY,
    fn_name TEXT NOT NULL,

    FOREIGN KEY (link_id)
        REFERENCES links(link_id)
        ON DELETE CASCADE
) STRICT;

--------------------------------------------------------------------------------
-- Dispatchers
--------------------------------------------------------------------------------

-- Dispatchers for XDP/TC multi-program chaining.
--
-- Natural key (type, nsid, ifindex) is the primary key - this is how
-- the system identifies a dispatcher ("the XDP dispatcher for this
-- interface"). program_id has a UNIQUE constraint so that
-- link_xdp_details and link_tc_details can use it as a FK target.
--
-- No FK back to managed_programs: this is deliberate, giving
-- flexibility in lifecycle ordering (the dispatcher row may be
-- created before or after the corresponding managed_programs row).
CREATE TABLE IF NOT EXISTS dispatchers (
    type TEXT NOT NULL CHECK (type IN ('xdp', 'tc-ingress', 'tc-egress')),
    nsid INTEGER NOT NULL,
    ifindex INTEGER NOT NULL,
    revision INTEGER NOT NULL DEFAULT 1 CHECK (revision >= 1),
    program_id INTEGER NOT NULL UNIQUE,
    link_id INTEGER NOT NULL DEFAULT 0,
    priority INTEGER NOT NULL DEFAULT 0 CHECK (priority >= 0),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,

    PRIMARY KEY (type, nsid, ifindex),

    -- XDP dispatchers have a link; TC dispatchers do not (uses netlink filters)
    CHECK (
        (type = 'xdp' AND link_id != 0)
        OR
        (type IN ('tc-ingress', 'tc-egress') AND link_id = 0)
    )
) STRICT;

--------------------------------------------------------------------------------
-- Dispatcher Extension Detail Tables
--------------------------------------------------------------------------------

-- XDP links (dispatcher-based)
CREATE TABLE IF NOT EXISTS link_xdp_details (
    link_id INTEGER PRIMARY KEY,
    interface TEXT NOT NULL,
    ifindex INTEGER NOT NULL,
    priority INTEGER NOT NULL CHECK (priority >= 0),
    position INTEGER NOT NULL CHECK (position BETWEEN 0 AND 9),
    proceed_on TEXT NOT NULL CHECK (json_valid(proceed_on)),
    netns TEXT,
    nsid INTEGER NOT NULL,
    dispatcher_program_id INTEGER NOT NULL,
    revision INTEGER NOT NULL CHECK (revision >= 1),

    FOREIGN KEY (link_id)
        REFERENCES links(link_id)
        ON DELETE CASCADE,
    -- ON DELETE CASCADE: removing a dispatcher removes extension link
    -- detail rows that reference it; without the dispatcher those
    -- extensions cannot run.
    -- ON UPDATE CASCADE: when a dispatcher is recompiled and gets a
    -- new kernel program_id, this FK is automatically updated so that
    -- every extension link row tracks the new ID.
    FOREIGN KEY (dispatcher_program_id)
        REFERENCES dispatchers(program_id)
        ON DELETE CASCADE
        ON UPDATE CASCADE
) STRICT;

-- Enforce unique position per interface in namespace
CREATE UNIQUE INDEX IF NOT EXISTS uq_xdp_dispatcher_position
    ON link_xdp_details(nsid, ifindex, position);

-- TC links (dispatcher-based)
CREATE TABLE IF NOT EXISTS link_tc_details (
    link_id INTEGER PRIMARY KEY,
    interface TEXT NOT NULL,
    ifindex INTEGER NOT NULL,
    direction TEXT NOT NULL CHECK (direction IN ('ingress', 'egress')),
    priority INTEGER NOT NULL CHECK (priority >= 0),
    position INTEGER NOT NULL CHECK (position BETWEEN 0 AND 9),
    proceed_on TEXT NOT NULL CHECK (json_valid(proceed_on)),
    netns TEXT,
    nsid INTEGER NOT NULL,
    dispatcher_program_id INTEGER NOT NULL,
    revision INTEGER NOT NULL CHECK (revision >= 1),

    FOREIGN KEY (link_id)
        REFERENCES links(link_id)
        ON DELETE CASCADE,
    -- ON DELETE CASCADE: removing a dispatcher removes extension link
    -- detail rows that reference it; without the dispatcher those
    -- extensions cannot run.
    -- ON UPDATE CASCADE: when a dispatcher is recompiled and gets a
    -- new kernel program_id, this FK is automatically updated so that
    -- every extension link row tracks the new ID.
    FOREIGN KEY (dispatcher_program_id)
        REFERENCES dispatchers(program_id)
        ON DELETE CASCADE
        ON UPDATE CASCADE
) STRICT;

-- Enforce unique position per interface + direction in namespace
CREATE UNIQUE INDEX IF NOT EXISTS uq_tc_dispatcher_position
    ON link_tc_details(nsid, ifindex, direction, position);

-- TCX links (kernel multi-attach)
-- The kernel handles multi-program ordering natively for TCX, so no
-- dispatcher is needed. No dispatcher_program_id, no position column,
-- no dispatcher cascade behaviour. Cleaned up solely by the links
-- cascade from managed_programs.
CREATE TABLE IF NOT EXISTS link_tcx_details (
    link_id INTEGER PRIMARY KEY,
    interface TEXT NOT NULL,
    ifindex INTEGER NOT NULL,
    direction TEXT NOT NULL CHECK (direction IN ('ingress', 'egress')),
    priority INTEGER NOT NULL CHECK (priority >= 0),
    netns TEXT,
    nsid INTEGER NOT NULL,

    FOREIGN KEY (link_id)
        REFERENCES links(link_id)
        ON DELETE CASCADE
) STRICT;
