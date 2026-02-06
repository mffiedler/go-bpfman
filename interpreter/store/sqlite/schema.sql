-- Schema for bpfman SQLite database
-- This schema uses the registry + detail tables pattern for links,
-- providing both polymorphic access and type-specific constraints.

-- Programs table for managed BPF programs
-- A row exists only after successful load - no reservation/loading states.
-- Schema is normalised: individual columns for queryable fields, JSON only for opaque data.
CREATE TABLE IF NOT EXISTS managed_programs (
    kernel_id INTEGER PRIMARY KEY,
    program_name TEXT NOT NULL,
    program_type TEXT NOT NULL,
    object_path TEXT NOT NULL,
    pin_path TEXT NOT NULL,
    attach_func TEXT,
    global_data TEXT,            -- JSON map<string, bytes>, opaque
    map_owner_id INTEGER,        -- Self-reference: program that owns shared maps
    map_pin_path TEXT,           -- Directory where maps are pinned
    image_source TEXT,           -- JSON ImageSource struct, NULL if file-loaded
    owner TEXT,
    description TEXT,
    license TEXT,                -- ELF license string from bytecode
    gpl_compatible INTEGER NOT NULL DEFAULT 0,
    metadata_json TEXT NOT NULL DEFAULT '{}', -- User key-value metadata as JSON
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,

    FOREIGN KEY (map_owner_id)
        REFERENCES managed_programs(kernel_id)
        ON DELETE RESTRICT       -- Prevent deleting owner while dependents exist
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
    kind            TEXT NOT NULL,        -- LinkKind discriminator
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

    FOREIGN KEY (kernel_prog_id)
        REFERENCES managed_programs(kernel_id)
        ON DELETE CASCADE
) STRICT;

CREATE INDEX IF NOT EXISTS idx_links_by_prog ON links(kernel_prog_id);
CREATE INDEX IF NOT EXISTS idx_links_by_kind ON links(kind);
CREATE INDEX IF NOT EXISTS idx_links_by_pin ON links(pin_path);

--------------------------------------------------------------------------------
-- Type-Specific Detail Tables
--------------------------------------------------------------------------------

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
    offset INTEGER NOT NULL DEFAULT 0,
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
    offset INTEGER NOT NULL DEFAULT 0,
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

-- Dispatchers table for XDP/TC multi-program chaining
-- Natural key (type, nsid, ifindex) is the primary key - this is how
-- the system identifies a dispatcher ("the XDP dispatcher for this interface").
-- kernel_id is the kernel-assigned program ID for the dispatcher program.
CREATE TABLE IF NOT EXISTS dispatchers (
    type TEXT NOT NULL CHECK (type IN ('xdp', 'tc-ingress', 'tc-egress')),
    nsid INTEGER NOT NULL,
    ifindex INTEGER NOT NULL,
    revision INTEGER NOT NULL DEFAULT 1,
    kernel_id INTEGER NOT NULL UNIQUE,
    link_id INTEGER NOT NULL DEFAULT 0,
    priority INTEGER NOT NULL DEFAULT 0,
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
    dispatcher_kernel_id INTEGER NOT NULL,
    revision INTEGER NOT NULL,

    FOREIGN KEY (link_id)
        REFERENCES links(link_id)
        ON DELETE CASCADE,
    FOREIGN KEY (dispatcher_kernel_id)
        REFERENCES dispatchers(kernel_id)
        ON DELETE CASCADE
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
    dispatcher_kernel_id INTEGER NOT NULL,
    revision INTEGER NOT NULL,

    FOREIGN KEY (link_id)
        REFERENCES links(link_id)
        ON DELETE CASCADE,
    FOREIGN KEY (dispatcher_kernel_id)
        REFERENCES dispatchers(kernel_id)
        ON DELETE CASCADE
) STRICT;

-- Enforce unique position per interface + direction in namespace
CREATE UNIQUE INDEX IF NOT EXISTS uq_tc_dispatcher_position
    ON link_tc_details(nsid, ifindex, direction, position);

-- TCX links (kernel multi-attach)
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
