// SPDX-License-Identifier: GPL-2.0
/*
 * bpfman_e2e_targets: dedicated kmod-backed attach targets for the
 * bpfman e2e test suite. Exports a fixed pool of noinline functions that
 * tests attach BPF programs to, plus per-slot debugfs trigger
 * files that invoke the corresponding function on write(2).
 *
 * The pool gives each fentry/fexit and kprobe/kretprobe test a
 * leased kernel-function slot with its own BPF trampoline, eliminating
 * the rebuild contention that sharing a common function (e.g. do_unlinkat)
 * introduces when several parallel tests attach and detach concurrently.
 * The lease is a test-harness convention, not kernel access control.
 *
 * See docs/HERMETIC-FENTRY-FEXIT-KMOD.md in the bpfman tree.
 */

#include <linux/module.h>
#include <linux/debugfs.h>
#include <linux/fs.h>
#include <linux/atomic.h>
#include <linux/uaccess.h>

#define BPFMAN_E2E_NUM_SLOTS 32

/*
 * X-macro that expands to a list of slot indices. Used to keep the
 * function definitions, the function-pointer table, and any future
 * per-slot state in sync without hand-maintaining 32-line blocks.
 */
#define BPFMAN_E2E_FOR_EACH_SLOT(X)                                  \
	X(0)  X(1)  X(2)  X(3)  X(4)  X(5)  X(6)  X(7)               \
	X(8)  X(9)  X(10) X(11) X(12) X(13) X(14) X(15)              \
	X(16) X(17) X(18) X(19) X(20) X(21) X(22) X(23)              \
	X(24) X(25) X(26) X(27) X(28) X(29) X(30) X(31)

/*
 * Each target is noinline so a real symbol exists in kallsyms and
 * BTF, and the asm volatile barrier prevents the compiler from
 * folding the body away. notrace is deliberately NOT set: BPF
 * fentry/fexit attach goes through the same ftrace machinery that
 * notrace excludes a function from, so marking these notrace would
 * make them unattachable.
 */
#define BPFMAN_E2E_DECLARE_TARGET(n)                                 \
	noinline long bpfman_e2e_target_##n(unsigned long arg);
BPFMAN_E2E_FOR_EACH_SLOT(BPFMAN_E2E_DECLARE_TARGET)
#undef BPFMAN_E2E_DECLARE_TARGET

#define BPFMAN_E2E_DEFINE_TARGET(n)                                  \
	noinline long bpfman_e2e_target_##n(unsigned long arg)       \
	{                                                            \
		asm volatile("" : : "r"(arg));                       \
		return (long)arg;                                    \
	}
BPFMAN_E2E_FOR_EACH_SLOT(BPFMAN_E2E_DEFINE_TARGET)
#undef BPFMAN_E2E_DEFINE_TARGET

typedef long (*bpfman_e2e_target_fn)(unsigned long);

struct bpfman_e2e_slot {
	bpfman_e2e_target_fn fn;
	atomic64_t trigger_count;
};

#define BPFMAN_E2E_SLOT_ENTRY(n)                                     \
	{ .fn = bpfman_e2e_target_##n, .trigger_count = ATOMIC64_INIT(0) },
static struct bpfman_e2e_slot bpfman_e2e_slots[BPFMAN_E2E_NUM_SLOTS] = {
	BPFMAN_E2E_FOR_EACH_SLOT(BPFMAN_E2E_SLOT_ENTRY)
};
#undef BPFMAN_E2E_SLOT_ENTRY

static struct dentry *bpfman_e2e_root;

/*
 * Any write(2) to a trigger file invokes the corresponding target
 * function exactly once and returns the byte count to satisfy
 * standard write semantics. Buffer contents are ignored: the test
 * harness drives event count by issuing N write calls, not by
 * encoding N in the buffer. This keeps the kernel side trivial
 * (no parsing, no copy_from_user) and the per-call overhead at
 * one syscall plus one indirect call.
 */
static ssize_t bpfman_e2e_trigger_write(struct file *file,
					const char __user *buf,
					size_t count, loff_t *ppos)
{
	struct bpfman_e2e_slot *slot = file->private_data;

	if (!slot || !slot->fn)
		return -EINVAL;
	(void)slot->fn(0);
	atomic64_inc(&slot->trigger_count);
	return count;
}

static ssize_t bpfman_e2e_count_read(struct file *file,
				     char __user *buf,
				     size_t count, loff_t *ppos)
{
	struct bpfman_e2e_slot *slot = file->private_data;
	char tmp[32];
	int len;

	if (!slot)
		return -EINVAL;

	len = scnprintf(tmp, sizeof(tmp), "%lld\n",
			(long long)atomic64_read(&slot->trigger_count));
	return simple_read_from_buffer(buf, count, ppos, tmp, len);
}

static const struct file_operations bpfman_e2e_trigger_fops = {
	.owner   = THIS_MODULE,
	.open    = simple_open,
	.write   = bpfman_e2e_trigger_write,
	.llseek  = noop_llseek,
};

static const struct file_operations bpfman_e2e_count_fops = {
	.owner   = THIS_MODULE,
	.open    = simple_open,
	.read    = bpfman_e2e_count_read,
	.llseek  = noop_llseek,
};

static int __init bpfman_e2e_init(void)
{
	int i;
	char name[32];

	bpfman_e2e_root = debugfs_create_dir("bpfman_e2e", NULL);
	if (IS_ERR(bpfman_e2e_root))
		return PTR_ERR(bpfman_e2e_root);

	for (i = 0; i < BPFMAN_E2E_NUM_SLOTS; i++) {
		snprintf(name, sizeof(name), "trigger_%03d", i);
		debugfs_create_file(name, 0600, bpfman_e2e_root,
				    &bpfman_e2e_slots[i],
				    &bpfman_e2e_trigger_fops);

		snprintf(name, sizeof(name), "count_%03d", i);
		debugfs_create_file(name, 0400, bpfman_e2e_root,
				    &bpfman_e2e_slots[i],
				    &bpfman_e2e_count_fops);
	}

	pr_info("bpfman_e2e_targets: %d slots ready under /sys/kernel/debug/bpfman_e2e/\n",
		BPFMAN_E2E_NUM_SLOTS);
	return 0;
}

static void __exit bpfman_e2e_exit(void)
{
	debugfs_remove_recursive(bpfman_e2e_root);
	pr_info("bpfman_e2e_targets: unloaded\n");
}

module_init(bpfman_e2e_init);
module_exit(bpfman_e2e_exit);

MODULE_LICENSE("GPL");
MODULE_AUTHOR("bpfman authors");
MODULE_DESCRIPTION("Dedicated kmod-backed attach targets for bpfman e2e tests");
MODULE_VERSION("0.1");
