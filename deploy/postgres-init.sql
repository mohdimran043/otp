-- The databases the test suite expects.
--
-- Three of them: one for the sender, one for the receiver, and one the unit-level integration tests
-- clone their per-test databases from. The two applications get separate databases because that is what
-- they have in production, and a suite that shared one would not notice code which assumed otherwise.
CREATE DATABASE otp_receiver;
CREATE DATABASE otp_test;
