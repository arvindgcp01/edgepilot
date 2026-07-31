-- Reverse 0009.
--
-- role_permissions first: the foreign key to permissions is what would otherwise
-- refuse the delete, and dropping the grants is also what makes the rollback
-- meaningful — a permission row nobody holds is harmless, a dangling grant is
-- not.

DELETE FROM role_permissions
WHERE permission_id IN (
    SELECT id FROM permissions
    WHERE slug IN ('enrollment_token:read', 'enrollment_token:write')
);

DELETE FROM permissions
WHERE slug IN ('enrollment_token:read', 'enrollment_token:write');
