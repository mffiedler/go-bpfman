package sqlite

import (
	"context"
	"fmt"
)

// prepareProgramStatements prepares all program-related SQL statements.
func (s *sqliteStore) prepareProgramStatements(ctx context.Context) error {
	var err error

	const sqlGetProgram = `
		SELECT m.program_name, m.program_type, m.object_path, m.pin_path, m.attach_func,
		       m.global_data, m.map_owner_id, m.map_pin_path, m.image_source, m.owner, m.description,
		       m.license, m.gpl_compatible, m.created_at, m.updated_at, m.metadata_json
		FROM managed_programs m
		WHERE m.program_id = ?`
	if s.stmtGetProgram, err = s.db.PrepareContext(ctx, sqlGetProgram); err != nil {
		return fmt.Errorf("prepare GetProgram: %w", err)
	}

	// Save uses upsert semantics: insert a new row, or overwrite an
	// existing row that has the same program_id. This is necessary
	// because the kernel can reuse program IDs aggressively after
	// unload, so a program_id collision does not necessarily indicate
	// a bug -- it may simply mean the ID was recycled. The store
	// treats Save as "last write wins" and the DB as a cache of
	// currently managed kernel objects.
	//
	// On conflict, created_at is deliberately preserved from the
	// original row so it records when the program_id first appeared
	// in the store. updated_at is set to the caller-supplied
	// timestamp so that created_at != updated_at serves as a clear
	// signal that a program_id was reused and the row was overwritten.
	const sqlSaveProgram = `
		INSERT INTO managed_programs
		(program_id, program_name, program_type, object_path, pin_path, attach_func,
		 global_data, map_owner_id, map_pin_path, image_source, owner, description, license, gpl_compatible, metadata_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(program_id) DO UPDATE SET
		  program_name = excluded.program_name,
		  program_type = excluded.program_type,
		  object_path = excluded.object_path,
		  pin_path = excluded.pin_path,
		  attach_func = excluded.attach_func,
		  global_data = excluded.global_data,
		  map_owner_id = excluded.map_owner_id,
		  map_pin_path = excluded.map_pin_path,
		  image_source = excluded.image_source,
		  owner = excluded.owner,
		  description = excluded.description,
		  license = excluded.license,
		  gpl_compatible = excluded.gpl_compatible,
		  metadata_json = excluded.metadata_json,
		  updated_at = excluded.updated_at`
	if s.stmtSaveProgram, err = s.db.PrepareContext(ctx, sqlSaveProgram); err != nil {
		return fmt.Errorf("prepare SaveProgram: %w", err)
	}

	const sqlDeleteProgram = "DELETE FROM managed_programs WHERE program_id = ?"
	if s.stmtDeleteProgram, err = s.db.PrepareContext(ctx, sqlDeleteProgram); err != nil {
		return fmt.Errorf("prepare DeleteProgram: %w", err)
	}

	const sqlListPrograms = `
		SELECT m.program_id, m.program_name, m.program_type, m.object_path, m.pin_path, m.attach_func,
		       m.global_data, m.map_owner_id, m.map_pin_path, m.image_source, m.owner, m.description,
		       m.license, m.gpl_compatible, m.created_at, m.updated_at, m.metadata_json
		FROM managed_programs m`
	if s.stmtListPrograms, err = s.db.PrepareContext(ctx, sqlListPrograms); err != nil {
		return fmt.Errorf("prepare ListPrograms: %w", err)
	}

	const sqlCountDependentPrograms = "SELECT COUNT(*) FROM managed_programs WHERE map_owner_id = ?"
	if s.stmtCountDependentPrograms, err = s.db.PrepareContext(ctx, sqlCountDependentPrograms); err != nil {
		return fmt.Errorf("prepare CountDependentPrograms: %w", err)
	}

	return nil
}

// prepareLinkRegistryStatements prepares all link registry SQL statements.
func (s *sqliteStore) prepareLinkRegistryStatements(ctx context.Context) error {
	var err error

	const sqlDeleteLink = "DELETE FROM links WHERE link_id = ?"
	if s.stmtDeleteLink, err = s.db.PrepareContext(ctx, sqlDeleteLink); err != nil {
		return fmt.Errorf("prepare DeleteLink: %w", err)
	}

	const sqlGetLinkRegistry = `
		SELECT link_id, kind, kernel_prog_id, pin_path, is_synthetic, created_at
		FROM links WHERE link_id = ?`
	if s.stmtGetLinkRegistry, err = s.db.PrepareContext(ctx, sqlGetLinkRegistry); err != nil {
		return fmt.Errorf("prepare GetLinkRegistry: %w", err)
	}

	const sqlListLinks = `
		SELECT link_id, kind, kernel_prog_id, pin_path, is_synthetic, created_at
		FROM links`
	if s.stmtListLinks, err = s.db.PrepareContext(ctx, sqlListLinks); err != nil {
		return fmt.Errorf("prepare ListLinks: %w", err)
	}

	const sqlListLinksByProgram = `
		SELECT link_id, kind, kernel_prog_id, pin_path, is_synthetic, created_at
		FROM links WHERE kernel_prog_id = ?`
	if s.stmtListLinksByProgram, err = s.db.PrepareContext(ctx, sqlListLinksByProgram); err != nil {
		return fmt.Errorf("prepare ListLinksByProgram: %w", err)
	}

	// link_id is the primary key (kernel ID or synthetic ID)
	// Caller provides the ID explicitly
	const sqlInsertLinkRegistry = `
		INSERT INTO links (link_id, kind, kernel_prog_id, pin_path, is_synthetic, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(link_id) DO UPDATE SET pin_path = excluded.pin_path`
	if s.stmtInsertLinkRegistry, err = s.db.PrepareContext(ctx, sqlInsertLinkRegistry); err != nil {
		return fmt.Errorf("prepare InsertLinkRegistry: %w", err)
	}

	const sqlListTCXLinksByInterface = `
		SELECT l.link_id, l.kernel_prog_id, td.priority
		FROM links l
		JOIN link_tcx_details td ON l.link_id = td.link_id
		WHERE td.nsid = ? AND td.ifindex = ? AND td.direction = ?
		ORDER BY td.priority ASC`
	if s.stmtListTCXLinksByInterface, err = s.db.PrepareContext(ctx, sqlListTCXLinksByInterface); err != nil {
		return fmt.Errorf("prepare ListTCXLinksByInterface: %w", err)
	}

	return nil
}

// prepareLinkDetailStatements prepares all link detail SQL statements.
func (s *sqliteStore) prepareLinkDetailStatements(ctx context.Context) error {
	var err error

	// Get statements
	const sqlGetTracepointDetails = "SELECT tp_group, tp_name FROM link_tracepoint_details WHERE link_id = ?"
	if s.stmtGetTracepointDetails, err = s.db.PrepareContext(ctx, sqlGetTracepointDetails); err != nil {
		return fmt.Errorf("prepare GetTracepointDetails: %w", err)
	}

	const sqlGetKprobeDetails = "SELECT fn_name, offset, retprobe FROM link_kprobe_details WHERE link_id = ?"
	if s.stmtGetKprobeDetails, err = s.db.PrepareContext(ctx, sqlGetKprobeDetails); err != nil {
		return fmt.Errorf("prepare GetKprobeDetails: %w", err)
	}

	const sqlGetUprobeDetails = "SELECT target, fn_name, offset, pid, retprobe FROM link_uprobe_details WHERE link_id = ?"
	if s.stmtGetUprobeDetails, err = s.db.PrepareContext(ctx, sqlGetUprobeDetails); err != nil {
		return fmt.Errorf("prepare GetUprobeDetails: %w", err)
	}

	const sqlGetFentryDetails = "SELECT fn_name FROM link_fentry_details WHERE link_id = ?"
	if s.stmtGetFentryDetails, err = s.db.PrepareContext(ctx, sqlGetFentryDetails); err != nil {
		return fmt.Errorf("prepare GetFentryDetails: %w", err)
	}

	const sqlGetFexitDetails = "SELECT fn_name FROM link_fexit_details WHERE link_id = ?"
	if s.stmtGetFexitDetails, err = s.db.PrepareContext(ctx, sqlGetFexitDetails); err != nil {
		return fmt.Errorf("prepare GetFexitDetails: %w", err)
	}

	const sqlGetXDPDetails = `
		SELECT interface, ifindex, priority, position, proceed_on, netns, nsid, dispatcher_program_id, revision
		FROM link_xdp_details WHERE link_id = ?`
	if s.stmtGetXDPDetails, err = s.db.PrepareContext(ctx, sqlGetXDPDetails); err != nil {
		return fmt.Errorf("prepare GetXDPDetails: %w", err)
	}

	const sqlGetTCDetails = `
		SELECT interface, ifindex, direction, priority, position, proceed_on, netns, nsid, dispatcher_program_id, revision
		FROM link_tc_details WHERE link_id = ?`
	if s.stmtGetTCDetails, err = s.db.PrepareContext(ctx, sqlGetTCDetails); err != nil {
		return fmt.Errorf("prepare GetTCDetails: %w", err)
	}

	const sqlGetTCXDetails = `
		SELECT interface, ifindex, direction, priority, netns, nsid,
			(SELECT COUNT(*) FROM link_tcx_details t2
			 WHERE t2.nsid = link_tcx_details.nsid
			 AND t2.ifindex = link_tcx_details.ifindex
			 AND t2.direction = link_tcx_details.direction
			 AND (t2.priority < link_tcx_details.priority
			      OR (t2.priority = link_tcx_details.priority AND t2.link_id < link_tcx_details.link_id))
			) AS position
		FROM link_tcx_details WHERE link_id = ?`
	if s.stmtGetTCXDetails, err = s.db.PrepareContext(ctx, sqlGetTCXDetails); err != nil {
		return fmt.Errorf("prepare GetTCXDetails: %w", err)
	}

	// Save statements
	const sqlSaveTracepointDetails = `
		INSERT INTO link_tracepoint_details (link_id, tp_group, tp_name)
		VALUES (?, ?, ?)`
	if s.stmtSaveTracepointDetails, err = s.db.PrepareContext(ctx, sqlSaveTracepointDetails); err != nil {
		return fmt.Errorf("prepare SaveTracepointDetails: %w", err)
	}

	const sqlSaveKprobeDetails = `
		INSERT INTO link_kprobe_details (link_id, fn_name, offset, retprobe)
		VALUES (?, ?, ?, ?)`
	if s.stmtSaveKprobeDetails, err = s.db.PrepareContext(ctx, sqlSaveKprobeDetails); err != nil {
		return fmt.Errorf("prepare SaveKprobeDetails: %w", err)
	}

	const sqlSaveUprobeDetails = `
		INSERT INTO link_uprobe_details (link_id, target, fn_name, offset, pid, retprobe)
		VALUES (?, ?, ?, ?, ?, ?)`
	if s.stmtSaveUprobeDetails, err = s.db.PrepareContext(ctx, sqlSaveUprobeDetails); err != nil {
		return fmt.Errorf("prepare SaveUprobeDetails: %w", err)
	}

	const sqlSaveFentryDetails = `
		INSERT INTO link_fentry_details (link_id, fn_name)
		VALUES (?, ?)`
	if s.stmtSaveFentryDetails, err = s.db.PrepareContext(ctx, sqlSaveFentryDetails); err != nil {
		return fmt.Errorf("prepare SaveFentryDetails: %w", err)
	}

	const sqlSaveFexitDetails = `
		INSERT INTO link_fexit_details (link_id, fn_name)
		VALUES (?, ?)`
	if s.stmtSaveFexitDetails, err = s.db.PrepareContext(ctx, sqlSaveFexitDetails); err != nil {
		return fmt.Errorf("prepare SaveFexitDetails: %w", err)
	}

	const sqlSaveXDPDetails = `
		INSERT INTO link_xdp_details (link_id, interface, ifindex, priority, position, proceed_on, netns, nsid, dispatcher_program_id, revision)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if s.stmtSaveXDPDetails, err = s.db.PrepareContext(ctx, sqlSaveXDPDetails); err != nil {
		return fmt.Errorf("prepare SaveXDPDetails: %w", err)
	}

	const sqlSaveTCDetails = `
		INSERT INTO link_tc_details (link_id, interface, ifindex, direction, priority, position, proceed_on, netns, nsid, dispatcher_program_id, revision)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if s.stmtSaveTCDetails, err = s.db.PrepareContext(ctx, sqlSaveTCDetails); err != nil {
		return fmt.Errorf("prepare SaveTCDetails: %w", err)
	}

	const sqlSaveTCXDetails = `
		INSERT INTO link_tcx_details (link_id, interface, ifindex, direction, priority, netns, nsid)
		VALUES (?, ?, ?, ?, ?, ?, ?)`
	if s.stmtSaveTCXDetails, err = s.db.PrepareContext(ctx, sqlSaveTCXDetails); err != nil {
		return fmt.Errorf("prepare SaveTCXDetails: %w", err)
	}

	// Batch list statements for populating link details in ListLinks
	const sqlListAllTracepointDetails = "SELECT link_id, tp_group, tp_name FROM link_tracepoint_details"
	if s.stmtListAllTracepointDetails, err = s.db.PrepareContext(ctx, sqlListAllTracepointDetails); err != nil {
		return fmt.Errorf("prepare ListAllTracepointDetails: %w", err)
	}

	const sqlListAllKprobeDetails = "SELECT link_id, fn_name, offset, retprobe FROM link_kprobe_details"
	if s.stmtListAllKprobeDetails, err = s.db.PrepareContext(ctx, sqlListAllKprobeDetails); err != nil {
		return fmt.Errorf("prepare ListAllKprobeDetails: %w", err)
	}

	const sqlListAllUprobeDetails = "SELECT link_id, target, fn_name, offset, pid, retprobe FROM link_uprobe_details"
	if s.stmtListAllUprobeDetails, err = s.db.PrepareContext(ctx, sqlListAllUprobeDetails); err != nil {
		return fmt.Errorf("prepare ListAllUprobeDetails: %w", err)
	}

	const sqlListAllFentryDetails = "SELECT link_id, fn_name FROM link_fentry_details"
	if s.stmtListAllFentryDetails, err = s.db.PrepareContext(ctx, sqlListAllFentryDetails); err != nil {
		return fmt.Errorf("prepare ListAllFentryDetails: %w", err)
	}

	const sqlListAllFexitDetails = "SELECT link_id, fn_name FROM link_fexit_details"
	if s.stmtListAllFexitDetails, err = s.db.PrepareContext(ctx, sqlListAllFexitDetails); err != nil {
		return fmt.Errorf("prepare ListAllFexitDetails: %w", err)
	}

	const sqlListAllXDPDetails = `
		SELECT link_id, interface, ifindex, priority, position, proceed_on, netns, nsid, dispatcher_program_id, revision
		FROM link_xdp_details`
	if s.stmtListAllXDPDetails, err = s.db.PrepareContext(ctx, sqlListAllXDPDetails); err != nil {
		return fmt.Errorf("prepare ListAllXDPDetails: %w", err)
	}

	const sqlListAllTCDetails = `
		SELECT link_id, interface, ifindex, direction, priority, position, proceed_on, netns, nsid, dispatcher_program_id, revision
		FROM link_tc_details`
	if s.stmtListAllTCDetails, err = s.db.PrepareContext(ctx, sqlListAllTCDetails); err != nil {
		return fmt.Errorf("prepare ListAllTCDetails: %w", err)
	}

	const sqlListAllTCXDetails = `
		SELECT link_id, interface, ifindex, direction, priority, netns, nsid,
			(SELECT COUNT(*) FROM link_tcx_details t2
			 WHERE t2.nsid = link_tcx_details.nsid
			 AND t2.ifindex = link_tcx_details.ifindex
			 AND t2.direction = link_tcx_details.direction
			 AND (t2.priority < link_tcx_details.priority
			      OR (t2.priority = link_tcx_details.priority AND t2.link_id < link_tcx_details.link_id))
			) AS position
		FROM link_tcx_details`
	if s.stmtListAllTCXDetails, err = s.db.PrepareContext(ctx, sqlListAllTCXDetails); err != nil {
		return fmt.Errorf("prepare ListAllTCXDetails: %w", err)
	}

	return nil
}
