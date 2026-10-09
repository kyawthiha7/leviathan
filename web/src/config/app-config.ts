import packageJson from "../../package.json";

const currentYear = new Date().getFullYear();

export const APP_CONFIG = {
  name: "LEVIATHAN",
  version: packageJson.version,
  copyright: `© ${currentYear}, LEVIATHAN.`,
  meta: {
    title: "LEVIATHAN: Autonomous Penetration Testing Console",
    description: "LLM Driver-Based Autonomous Penetration Testing System Console",
  },
};
