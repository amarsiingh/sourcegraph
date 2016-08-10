// tslint:disable: typedef ordered-imports curly

import * as React from "react";
import * as styles from "./styles/table.css";
import * as classNames from "classnames";

type Props = {
	className?: string,
	children?: any,
	bordered?: boolean,
	style?: any,
};

export class Table extends React.Component<Props, any> {
	render(): JSX.Element | null {
		const {className, children, bordered, style} = this.props;

		return (
			<table className={classNames(className, bordered && styles.bordered)} style={style} cellSpacing="0">
				{children}
			</table>
		);
	}
}
