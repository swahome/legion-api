-- +goose Up

-- A member's request for a loan (legacy loan_applications). It goes through
-- the guarantors the member named, then the group's signatories, then an
-- official pays it out, which records the loan.
CREATE TYPE loan_application_status AS ENUM (
    'pending_guarantors', -- waiting on the guarantors (legacy status 2)
    'pending_approval',   -- waiting on the signatories (3)
    'approved',           -- waiting to be paid out (4)
    'disbursed',          -- paid out: loan_id is the loan
    'declined',           -- a guarantor or signatory said no, or an official declined it (8)
    'cancelled'           -- the member withdrew it
);

CREATE TABLE loan_applications (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    group_id       bigint NOT NULL REFERENCES groups (id),
    membership_id  bigint NOT NULL,
    loan_type_id   bigint NOT NULL,
    amount         numeric(14, 2) NOT NULL CHECK (amount > 0),
    installments   integer NOT NULL CHECK (installments > 0),
    description    text NOT NULL DEFAULT '',
    status         loan_application_status NOT NULL,
    decline_reason text NOT NULL DEFAULT '',
    decided_at     timestamptz,     -- approved, declined or cancelled
    loan_id        bigint,          -- set when it is paid out
    created_by     bigint REFERENCES users (id),
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (group_id, id),
    FOREIGN KEY (group_id, membership_id) REFERENCES memberships (group_id, id),
    FOREIGN KEY (group_id, loan_type_id) REFERENCES loan_types (group_id, id),
    FOREIGN KEY (group_id, loan_id) REFERENCES loans (group_id, id)
);
CREATE INDEX loan_applications_group_idx ON loan_applications (group_id, created_at DESC, id DESC);
CREATE INDEX loan_applications_member_idx ON loan_applications (membership_id);

-- The members asked to guarantee an application (legacy
-- loan_guarantorship_requests). When the application is paid out, the ones
-- who agreed become the loan's guarantors.
CREATE TABLE loan_application_guarantors (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    group_id       bigint NOT NULL REFERENCES groups (id),
    application_id bigint NOT NULL,
    membership_id  bigint NOT NULL,
    amount         numeric(14, 2),
    comment        text NOT NULL DEFAULT '',
    decision       approval_decision NOT NULL DEFAULT 'pending',
    decided_at     timestamptz,
    reason         text NOT NULL DEFAULT '',
    UNIQUE (application_id, membership_id),
    FOREIGN KEY (group_id, application_id) REFERENCES loan_applications (group_id, id),
    FOREIGN KEY (group_id, membership_id) REFERENCES memberships (group_id, id)
);
CREATE INDEX loan_application_guarantors_member_idx ON loan_application_guarantors (membership_id, decision);

-- The signatories' answers (legacy loan_signatory_requests): every one must
-- approve, one decline declines.
CREATE TABLE loan_application_approvals (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    group_id       bigint NOT NULL REFERENCES groups (id),
    application_id bigint NOT NULL,
    membership_id  bigint NOT NULL,
    decision       approval_decision NOT NULL DEFAULT 'pending',
    decided_at     timestamptz,
    decided_by     bigint REFERENCES users (id),
    comment        text NOT NULL DEFAULT '',
    UNIQUE (application_id, membership_id),
    FOREIGN KEY (group_id, application_id) REFERENCES loan_applications (group_id, id),
    FOREIGN KEY (group_id, membership_id) REFERENCES memberships (group_id, id)
);
CREATE INDEX loan_application_approvals_member_idx ON loan_application_approvals (membership_id, decision);

-- +goose Down
DROP TABLE loan_application_approvals;
DROP TABLE loan_application_guarantors;
DROP TABLE loan_applications;
DROP TYPE loan_application_status;
