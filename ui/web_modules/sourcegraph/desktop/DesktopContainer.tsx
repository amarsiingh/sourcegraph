// tslint:disable: typedef ordered-imports curly

import * as React from "react";

import {Container} from "sourcegraph/Container";
import * as Dispatcher from "sourcegraph/Dispatcher";
import {DefStore} from "sourcegraph/def/DefStore";
import * as DefActions from "sourcegraph/def/DefActions";

import {urlToDefInfo} from "sourcegraph/def/routes";
import {urlToTree} from "sourcegraph/tree/routes";

export function desktopContainer(Component) {
	class DesktopContainer extends Container<{}, any> {
		static contextTypes = {
			router: React.PropTypes.object.isRequired,
		};

		constructor(props) {
			super(props);
			this.desktopNavigation = this.desktopNavigation.bind(this);
			this.state = {
				defSpec: {},
			};
		}

		stores() { return [DefStore]; }

		reconcileState(state, props) {
			Object.assign(state, props);
		}

		componentWillMount() {
			super.componentWillMount();
			document.addEventListener("sourcegraph:desktop:navToSym", this.desktopNavigation);
		}

		componentWillUnmount() {
			super.componentWillUnmount();
			document.removeEventListener("sourcegraph:desktop:navToSym", this.desktopNavigation);
		}

		onStateTransition(oldState, newState) {
			const defSpec = newState.defSpec;
			const def = DefStore.defs.get(defSpec.repo, null, defSpec.def);
			if (!def) { return; }
			if (def.Error) {
				messageDesktop(def.Error);
			} else {
				const url = urlToDefInfo(def);
				(this.context as any).router.push(url);
			}
			newState.defSpec = {};
		}

		desktopNavigation(event) {
			const info = event.detail;
			if (info.Kind === "package") {
				const url = urlToTree(info.repo, null, info.treePkg);
				(this.context as any).router.push(url);
				return;
			}
			info.def = infoToDef(info);
			this.setState({defSpec: {repo: info.repo, def: info.def}});
			Dispatcher.Backends.dispatch(new DefActions.WantDef(info.repo, null, info.def));
		}

		render() {
			return <Component {...this.props}/>;
		}
	}

	return DesktopContainer;
}

function infoToDef(info) {
	return `${info.UnitType}/${info.pkg}/-/${info.sym}`;
}

// This function sends a message to the desktop application. This is obviously
// not ideal, but it is the only practical way to send a message from the
// webview to the desktop app AFAICT.
function messageDesktop(message) {
	const json = JSON.stringify(message);
	// tslint:disable: no-console
	console.debug(json);
}
