-- Lock the restaurant row while checking its program, so a concurrent program
-- change cannot slip between this check and the order's commit.
--
-- FOR SHARE (not FOR KEY SHARE): the orders.restaurant_id foreign key already
-- takes FOR KEY SHARE, which only blocks key changes and deletes. Changing
-- loyalty_program_id is a non-key UPDATE; FOR SHARE blocks it. FOR SHARE locks
-- are compatible with each other, so concurrent order inserts do not serialize.
--
-- CREATE OR REPLACE keeps the existing trigger bound to this function.
CREATE OR REPLACE FUNCTION orders_program_matches_restaurant() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    restaurant_program bigint;
BEGIN
    SELECT loyalty_program_id INTO restaurant_program
    FROM restaurants WHERE id = NEW.restaurant_id
    FOR SHARE;

    IF restaurant_program IS DISTINCT FROM NEW.loyalty_program_id THEN
        RAISE EXCEPTION 'order loyalty_program_id % does not match restaurant % program %',
            NEW.loyalty_program_id, NEW.restaurant_id, restaurant_program
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;
