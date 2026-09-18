// Lint config for the plugin's source. The console-plugin-template this was
// scaffolded from also wired up prettier, import-x, jest, testing-library and
// playwright; none of those were ever installed here and there are no tests
// for them to lint, so they are left out rather than carried as dead weight.
import eslint from '@eslint/js';
import tseslint from 'typescript-eslint';
import react from 'eslint-plugin-react';
import reactHooks from 'eslint-plugin-react-hooks';
import globals from 'globals';

export default tseslint.config(
  {
    ignores: ['dist/', 'node_modules/'],
  },
  eslint.configs.recommended,
  // Type-aware correctness rules. The template's strict/stylistic sets flag
  // defensive checks on API data as "unnecessary", which they are not at
  // runtime; correctness is what a lint gate is for.
  tseslint.configs.recommendedTypeChecked,
  reactHooks.configs.flat['recommended-latest'],
  {
    files: ['src/**/*.{ts,tsx}'],
    plugins: {
      react,
    },
    rules: {
      ...react.configs.recommended.rules,
      ...react.configs['jsx-runtime'].rules,
      '@typescript-eslint/consistent-type-imports': 'error',
    },
    languageOptions: {
      globals: globals.browser,
      parserOptions: {
        projectService: true,
        ecmaFeatures: {
          jsx: true,
        },
      },
    },
    settings: {
      react: {
        version: 'detect',
      },
    },
  },
);
