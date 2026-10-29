CREATE TABLE users (
  id    serial PRIMARY KEY,
  name  text NOT NULL,
  email text NOT NULL
);
INSERT INTO users (name, email) VALUES
  ('Ada', 'ada@example.com'),
  ('Grace', 'grace@example.com'),
  ('Linus', 'linus@example.com');
